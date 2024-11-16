/*
 * @Author: amamiya-yuuko-1225 1913250675@qq.com
 * @Date: 2024-11-09 14:27:05
 * @LastEditors: amamiya-yuuko-1225 1913250675@qq.com
 * @Description:
 */
package main

import (
	"bufio"
	"bytes"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	//maxmimum number of connections
	MAX_CONN = 10
	//root directory for files
	FILE_DIR = "/home/amamiya"
)

var (
	//map file extenstions to content-type
	extMap = map[string]string{
		".html": "text/html",
		".txt":  "text/plain",
		".gif":  "image/gif",
		".jpeg": "image/jpeg",
		".jpg":  "image/jpeg",
		".css":  "text/css",
	}
	// control the degree of accepted connections
	connChan = make(chan int, MAX_CONN)
)

/**
 * @description: process GET request
 * @param {*http.Request} req
 * @return {*http.Response}:
 * nil: unexpected error; 400 Bad Request for unsupported file extensions
 * 404 Not Found for missing file; 200 OK
 */
func process_get_req(req *http.Request) *http.Response {
	//Get request path and file extension
	path := req.URL.Path
	ext := filepath.Ext(path)

	//Check if file extension supported
	contentType, ok := extMap[ext]
	if !ok {
		return &http.Response{
			Status:     "400 Bad Request",
			StatusCode: http.StatusBadRequest,
			Proto:      "HTTP/1.0",
			ProtoMajor: 1,
			ProtoMinor: 0,
			Header:     make(http.Header),
			Body:       nil,
		}
	}

	//Open target file
	file, err := os.Open(FILE_DIR + path)
	if err != nil {
		//file do not exists
		if _, ok := err.(*os.PathError); ok {
			return &http.Response{
				Status:     "404 Not Found",
				StatusCode: http.StatusNotFound,
				Proto:      "HTTP/1.0",
				ProtoMajor: 1,
				ProtoMinor: 0,
				Header:     make(http.Header),
				Body:       nil,
			}
		} else {
			log.Println(err)
			return nil
		}
	}
	defer file.Close()

	//Read target file
	data, err := io.ReadAll(file)
	if err != nil {
		log.Println(err)
		return nil
	}

	// Request successfully handled
	response := &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.0",
		ProtoMajor: 1,
		ProtoMinor: 0,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(data)),
	}
	response.Header.Set("Content-Type", contentType)
	return response
}

/**
 * @description: process POST request
 * @param {*http.Request} req
 * @return {*http.Response} nil: unexpected error; 200 OK
 */
func process_post_req(req *http.Request) *http.Response {
	//Open requst body
	reqBody := req.Body
	defer reqBody.Close()

	//Get request path, create an empty file for the target
	path := req.URL.Path
	outFile, err := os.Create(FILE_DIR + path)
	if err != nil {
		log.Println(err)
		return nil
	}
	defer outFile.Close()

	//Write request body to target file
	if _, err := io.Copy(outFile, reqBody); err != nil {
		log.Println(err)
		return nil
	}

	// Request successfully handled
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.0",
		ProtoMajor: 1,
		ProtoMinor: 0,
		Header:     make(http.Header),
		Body:       nil,
	}
}

/**
 * @description: deal with http request for a tcp connection
 * @param {net.Conn} conn: tcp connection
 * @return {*}
 */
func process_conn(conn net.Conn) {
	// to limit the number of connections using channel
	// if channel if full, the goroutine will be blocked
	connChan <- 1
	defer func() { <-connChan }()

	defer conn.Close() // close connection before exit
	// input buffer
	buf := make([]byte, 1024)
	_, err := conn.Read(buf) // read from connection
	if err != nil {
		log.Println(err)
		return
	}
	// conver byte[] to bufio.reader
	reader := bufio.NewReader(strings.NewReader(string(buf)))
	// parse request
	req, err := http.ReadRequest(reader)
	if err != nil {
		log.Println(err)
		return
	}

	var response *http.Response = nil
	// Only handle "GET" & "POST" request
	// Otherwise respond "501 Not Implemented"
	switch req.Method {
	case "GET":
		response = process_get_req(req)
	case "POST":
		response = process_post_req(req)
	default:
		response = &http.Response{
			Status:     "501 Not Implemented",
			StatusCode: http.StatusNotImplemented,
			Proto:      "HTTP/1.0",
			ProtoMajor: 1,
			ProtoMinor: 0,
			Header:     make(http.Header),
			Body:       nil,
		}
	}
	// Unexpected error ocurred when processing request
	if response == nil {
		log.Println(err)
		response = &http.Response{
			Status:     "500 Internal Server Error",
			StatusCode: http.StatusInternalServerError,
			Proto:      "HTTP/1.0",
			ProtoMajor: 1,
			ProtoMinor: 0,
			Header:     make(http.Header),
			Body:       nil,
		}
	}
	err = response.Write(conn)
	if err != nil {
		log.Println(err)
	}

}

func main() {

	//Get specified port from cmd, default 20000
	port := flag.String("p", "20000", "Specifiy listing port")
	flag.Parse()

	//Open listing socket
	listen, err := net.Listen("tcp", ":"+*port)
	if err != nil {
		log.Fatal(err)
		return
	}
	defer listen.Close()

	//Always listening
	for {
		conn, err := listen.Accept() // Establish tcp connection
		if err != nil {
			log.Println(err)
			continue
		}
		go process_conn(conn) // Create new connection socket in a new goroutine
	}
}
