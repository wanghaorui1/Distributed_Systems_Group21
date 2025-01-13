package mr

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"io/ioutil"
	"log"
	"net/rpc"
	"os"
	"sort"
	"time"
)

type ByKey []KeyValue

// for sorting by key.
func (a ByKey) Len() int           { return len(a) }
func (a ByKey) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByKey) Less(i, j int) bool { return a[i].Key < a[j].Key }

// Map functions return a slice of KeyValue.
type KeyValue struct {
	Key   string
	Value string
}

// use ihash(key) % NReduce to choose the reduce
// task number for each KeyValue emitted by Map.
func ihash(key string) int {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32() & 0x7fffffff)
}

// main/mrworker.go calls this function.
func Worker(mapf func(string, string) []KeyValue, reducef func(string, []string) string) {
	workerId := registerWorker()
	var args TaskRequest
	reply := TaskResponse{}
	reply.TaskType = "idle"
	for {
		// if all done, exit
		if reply.AllDone {
			// log.Printf("All done, exiting...")
			return
		}
		if reply.TaskType == "idle" {
			time.Sleep(10 * time.Millisecond) // avoid flooding the coordinator
			args = TaskRequest{WorkerState: Idle, WorkerId: workerId}
		} else if reply.TaskType == "map" {
			execMap(reply.FileName, mapf, reply.MapId-1, reply.NReduce)
			args = TaskRequest{WorkerState: MapFinished, WorkerId: workerId, MapId: reply.MapId - 1}
		} else {
			execReduce(reply.ReduceId-1, reducef, reply.MapCount)
			args = TaskRequest{WorkerState: ReduceFinished, WorkerId: workerId, ReduceId: reply.ReduceId - 1}
		}
		ok := call("Coordinator.AllocateTasks", &args, &reply)
		// Coordinator failure deteced through the return of RPC
		if !ok {
			log.Fatal("Coordinator failure detected, exiting...")
		}
	}
}

/**
 * @description: execute map task.
 * @param {string} filename: filename of the map task
 * @param {func} mapf: real map function
 * @param {int} mapId: the id of map task. Associated with file.
 * @param {int} nReduce: number of reduce tasks
 * @return {*}
 */
func execMap(filename string, mapf func(string, string) []KeyValue, mapId int, nReduce int) {
	log.Printf("map %d starts", mapId)
	// open original file.
	file, err := os.Open(filename)
	if err != nil {
		log.Fatalf("cannot open %v", filename)
	}
	content, err := ioutil.ReadAll(file)
	if err != nil {
		log.Fatalf("cannot read %v", filename)
	}
	file.Close()

	// do map task
	kvs := mapf(filename, string(content))

	//create intermediate JSON files
	intermediateFiles := make([]*os.File, nReduce)
	encoders := make([]*json.Encoder, nReduce)
	for i := 0; i < nReduce; i++ {
		// create temp files, in order to prevent corrupt data being fetched
		nameTemp := fmt.Sprintf("mr-%d-%d_", mapId, i)
		intermediateFiles[i], err = os.Create(nameTemp)
		if err != nil {
			log.Fatalf("cannot create file %v", nameTemp)
		}
		encoders[i] = json.NewEncoder(intermediateFiles[i])
	}
	// iterate all KVs, write to temp files
	for _, kv := range kvs {
		// allocate reducer Hash(key)
		reduceId := ihash(kv.Key) % nReduce
		// store KV into JSON file with index reduceID
		err := encoders[reduceId].Encode(&kv)
		if err != nil {
			log.Fatalf("cannot encode kv pair: %v", err)
		}

	}
	// move temp files to final map output files
	for _, tempFile := range intermediateFiles {
		nameTemp := tempFile.Name()
		name := nameTemp[:len(nameTemp)-1]
		if fileExists(name) {
			os.Remove(nameTemp)
		} else {
			os.Rename(nameTemp, name)
		}
	}
	log.Printf("map %d done", mapId)
}

/**
 * @description: execute reduce task
 * @param {int} reduceId: id of reduce task
 * @param {func} reducef: real reduce function
 * @param {int} nMap: number of files(map tasks)
 * @return {*}
 */
func execReduce(reduceId int, reducef func(string, []string) string, nMap int) {
	log.Printf("reduce %d starts", reduceId)
	// read all JSON files with specific reduceID
	// and convert them into KV[]
	intermediate := []KeyValue{}
	for i := 0; i < nMap; i++ {
		name := fmt.Sprintf("mr-%d-%d", i, reduceId)
		file, err := os.Open(name)
		if err != nil {
			log.Fatalf("cannot open file %v", name)
		}
		dec := json.NewDecoder(file)
		for {
			var kv KeyValue
			if err := dec.Decode(&kv); err != nil {
				if err == io.EOF {
					break
				} else {
					log.Fatalf("Decode error: %v", err)
				}
			}
			intermediate = append(intermediate, kv)
		}
		file.Close()
	}

	//temp reduce output
	oname := fmt.Sprintf("mr-out-%d.txt_", reduceId)
	ofile, _ := os.Create(oname)

	// Shuffling/Grouping
	sort.Sort(ByKey(intermediate))

	//Group all KVs with a same key, and pass to reduce function
	i := 0
	for i < len(intermediate) {
		j := i + 1
		for j < len(intermediate) && intermediate[j].Key == intermediate[i].Key {
			j++
		}
		values := []string{}
		// all KVs with a same key are grouped into "values"
		for k := i; k < j; k++ {
			values = append(values, intermediate[k].Value)
		}
		// do reduce task
		output := reducef(intermediate[i].Key, values)
		// write to temp output file
		fmt.Fprintf(ofile, "%v %v\n", intermediate[i].Key, output)
		i = j
	}
	// move temp file to final output file
	nameTemp := oname
	name := nameTemp[:len(nameTemp)-1]
	if fileExists(name) {
		os.Remove(nameTemp)
	} else {
		os.Rename(nameTemp, name)
	}

	log.Printf("reduce %d done", reduceId)
}

/**
 * @description: register this machine at coordinator
 * @return {int} workerID or -1
 */
func registerWorker() int {
	args := RegisterArgs{}
	reply := RegisterReply{}
	ok := call("Coordinator.RegisterWorker", &args, &reply)
	if ok {
		return reply.WorkerId
	}
	log.Fatal("Failed to register worker")
	return -1
}

// send an RPC request to the coordinator, wait for the response.
// usually returns true.
// returns false if something goes wrong.
func call(rpcname string, args interface{}, reply interface{}) bool {
	// c, err := rpc.DialHTTP("tcp", "127.0.0.1"+":1234")
	sockname := coordinatorSock()
	c, err := rpc.DialHTTP("unix", sockname)
	if err != nil {
		log.Fatal("dialing:", err)
	}
	defer c.Close()

	err = c.Call(rpcname, args, reply)
	if err == nil {
		return true
	}

	fmt.Println(err)
	return false
}
