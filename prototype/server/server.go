// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/gob"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path"
	"search/prototype/index"
	"search/prototype/logger"
	"search/prototype/searcher"
	"search/prototype/util"
	"strconv"
	"time"
)

// Server contains all the necessary information for a running server.
type Server struct {
	directory string        // Directory of the server
	lenMS     int           // Length of the master secret in bytes
	keyHalves [][]byte      // The server-side keyhalves
	salts     [][]byte      // The salts for deriving the keys for the PRFs
	numFiles  int           // The number of files currently stored in the server.  This is used to determine the next docID.
	size      uint64        // The number of slots in the bloom filter index
	latency   time.Duration // The latency between the server and the client
	bandwidth int           // The bandwidth of the link betweem the server and the client (in bps)
}

// CreateServer initializes a server with `numClients` clients with a master
// secret of length `lenMS`, and generate salts with length `lenSalt`.  The
// number of salts is given by `r = -log2(fpRate)`, where `fpRate` is the
// desired false positive rate of the system.  `directory` determines where the
// server files will be stored.  Returns an error if the salt cannot be properly
// generated.
func CreateServer(numClients, lenMS, lenSalt int, directory string, fpRate float64, numUniqWords uint64) (s *Server, err error) {
	s = new(Server)
	masterSecret := make([]byte, lenMS)
	rand.Read(masterSecret)
	s.keyHalves = make([][]byte, numClients)
	s.lenMS = lenMS
	for i := 0; i < numClients; i++ {
		h := sha256.New()
		h.Write([]byte(strconv.Itoa(i)))
		cksum := h.Sum(nil)
		s.keyHalves[i] = util.XorBytes(masterSecret, cksum, lenMS)
	}
	r := int(math.Ceil(-math.Log2(fpRate)))
	s.size = uint64(math.Ceil(float64(numUniqWords) * float64(r) / math.Log(2)))
	s.salts, err = util.GenerateSalts(r, lenSalt)
	if err != nil {
		return
	}
	s.numFiles = 0
	s.directory = directory
	s.writeToFile()
	return
}

// CreateServerWithNetConfig behaves the same as `CreateServer`, except for that it
// also sets the network parameters for the server.
func CreateServerWithNetConfig(numClients, lenMS, lenSalt int, directory string, fpRate float64, numUniqWords uint64, latency time.Duration, bandwidth int) (*Server, error) {
	s, err := CreateServer(numClients, lenMS, lenSalt, directory, fpRate, numUniqWords)
	s.latency = latency
	s.bandwidth = bandwidth
	s.writeToFile()
	return s, err
}

// LoadServer initializes a Server by reading the metadata stored at
// `directory` and restoring the server status.
func LoadServer(directory string) *Server {
	input, err := os.Open(path.Join(directory, "serverMD"))
	if err != nil {
		panic("Server metadata not found")
	}
	dec := gob.NewDecoder(input)

	s := new(Server)
	dec.Decode(&s.directory)
	dec.Decode(&s.numFiles)
	dec.Decode(&s.salts)
	dec.Decode(&s.keyHalves)
	dec.Decode(&s.lenMS)
	dec.Decode(&s.size)
	dec.Decode(&s.latency)
	dec.Decode(&s.bandwidth)

	input.Close()

	return s
}

// writeToFile serializes the server status and writes the metadata to a file in
// the server directory, which can be later loaded by `LoadServer`.
func (s *Server) writeToFile() {
	file, _ := os.Create(path.Join(s.directory, "serverMD"))
	enc := gob.NewEncoder(file)
	enc.Encode(s.directory)
	enc.Encode(s.numFiles)
	enc.Encode(s.salts)
	enc.Encode(s.keyHalves)
	enc.Encode(s.lenMS)
	enc.Encode(s.size)
	enc.Encode(s.latency)
	enc.Encode(s.bandwidth)

	file.Close()
}

// AddFile adds a file with `content` to the server with the document ID equal
// to the number of files currently in the server and updates the count.
// Returns the document ID.
func (s *Server) AddFile(content []byte) (int, error) {
	logger.AddTime(s.latency * 2)
	// The `*1.5` is included to account for the possible increase in file length
	// after excryption.
	logger.AddTime(time.Millisecond * time.Duration(float64(len(content))*1.5*8*1000/float64(s.bandwidth)))
	output, err := os.Create(path.Join(s.directory, strconv.Itoa(s.numFiles)))
	if err != nil {
		return 0, err
	}
	output.Write(content)
	s.numFiles++
	output.Close()
	s.writeToFile()
	return s.numFiles - 1, nil
}

// GetFile returns the content of the document with `docID`.  Behavior is
// undefined if the docID is invalid (out of range).
func (s *Server) GetFile(docID int) ([]byte, error) {
	logger.AddTime(s.latency * 2)
	content, err := ioutil.ReadFile(path.Join(s.directory, strconv.Itoa(docID)))
	if err != nil {
		return nil, err
	}
	logger.AddTime(time.Millisecond * time.Duration(float64(len(content))*1.5*8*1000/float64(s.bandwidth)))
	return content, nil
}

// WriteIndex writes a SecureIndex to the disk of the server.
func (s *Server) WriteIndex(si index.SecureIndex) error {
	logger.AddTime(s.latency * 2)
	output, err := si.MarshalBinary()
	if err != nil {
		return err
	}
	logger.AddTime(time.Millisecond * time.Duration(float64(len(output))*8*1000/float64(s.bandwidth)))
	file, err := os.Create(path.Join(s.directory, strconv.Itoa(si.DocID)+".index"))
	if err != nil {
		return err
	}
	file.Write(output)
	file.Close()
	return nil
}

// readIndex loads an index from the disk.
func (s *Server) readIndex(docID int) (si index.SecureIndex, err error) {
	input, err := ioutil.ReadFile(path.Join(s.directory, strconv.Itoa(docID)+".index"))
	if err != nil {
		return
	}
	err = si.UnmarshalBinary(input)
	return
}

// SearchWord searches the server for a word with `trapdoors`.  Returns a list
// of document ids of files possibly containing the word in increasing order.
func (s *Server) SearchWord(trapdoors [][]byte) []int {
	logger.AddTime(s.latency * 2)
	var result []int
	for i := 0; i < s.numFiles; i++ {
		si, err := s.readIndex(i)
		// Skip the file if there is an error reading the index
		if err != nil {
			continue
		}
		if searcher.SearchSecureIndex(si, trapdoors) {
			result = append(result, i)
		}
	}
	logger.AddTime(time.Millisecond * time.Duration(float64(len(trapdoors)*len(trapdoors[0])+len(result))*8*1000/float64(s.bandwidth)))
	return result
}

// WriteLookupTable writes `content` to the file "lookupTable".
func (s *Server) WriteLookupTable(content []byte) {
	logger.AddTime(s.latency * 2)
	logger.AddTime(time.Millisecond * time.Duration(float64(len(content))*1.5*8*1000/float64(s.bandwidth)))
	file, _ := os.Create(path.Join(s.directory, "lookupTable"))
	file.Write(content)
	file.Close()
}

// ReadLookupTable reads the content in the file "lookupTable" and returns it in
// a byte slice.  If not found, returns false as the second return value.
func (s *Server) ReadLookupTable() ([]byte, bool) {
	logger.AddTime(s.latency * 2)
	if _, err := os.Stat(path.Join(s.directory, "lookupTable")); os.IsNotExist(err) {
		return []byte{}, false
	}
	content, _ := ioutil.ReadFile(path.Join(s.directory, "lookupTable"))
	logger.AddTime(time.Millisecond * time.Duration(float64(len(content))*1.5*8*1000/float64(s.bandwidth)))
	return content, true
}

// GetNumClients returns the number of clients for this server.
func (s *Server) GetNumClients() int {
	logger.AddTime(s.latency * 2)
	return len(s.keyHalves)
}

// GetKeyHalf returns the server-side key half for client with `clientNum`.
// Behavior is undefined if `clientNum` is invalid (out of range).
func (s *Server) GetKeyHalf(clientNum int) []byte {
	logger.AddTime(s.latency * 2)
	logger.AddTime(time.Millisecond * time.Duration(float64(len(s.keyHalves[0]))*8*1000/float64(s.bandwidth)))
	return s.keyHalves[clientNum]
}

// GetSalts returns the salts to the client.
func (s *Server) GetSalts() [][]byte {
	logger.AddTime(s.latency * 2)
	logger.AddTime(time.Millisecond * time.Duration(float64(len(s.salts)*len(s.salts[0]))*8*1000/float64(s.bandwidth)))
	return s.salts
}

// GetSize returns the size of the indexes on the server.
func (s *Server) GetSize() uint64 {
	logger.AddTime(s.latency * 2)
	return s.size
}

// PrintServerInfo prints out the basic information of the server.
func (s *Server) PrintServerInfo() {
	fmt.Printf("Server ID: %.3x\n", s.keyHalves[0])
	fmt.Println("Directory:", s.directory)
	fmt.Println("Size:", s.size)
	fmt.Println("Number of Clients:", len(s.keyHalves))
	fmt.Println("Length of Master Secret:", s.lenMS)
	fmt.Println("Number of PRFs:", len(s.salts))
	fmt.Println("Server Latency:", s.latency.String())
	bwUnits := []string{"bps", "kbps", "mbps"}
	scale := 0
	bw := float64(s.bandwidth)
	for bw >= 1024 && scale < 2 {
		bw /= 1024
		scale++
	}
	fmt.Printf("Connection Bandwidth: %.1f%s\n", bw, bwUnits[scale])
	fmt.Println("Number of Files:", s.numFiles)
}
