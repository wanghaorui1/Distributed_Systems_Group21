package mr

import (
	"log"
	"net"
	"net/http"
	"net/rpc"
	"os"
	"sync"
	"time"
)

type TaskInfo struct {
	TaskType string // "map" or "reduce"
	Value    string // Could be status: UnAllocated, Allocated,
	//Finishied; or filename for map; or "" for reduce
	ID int
}

type MapTask struct {
	MapId    int
	FileName string
}

const (
	UnAllocated = iota
	Allocated
	Finished
)

type Coordinator struct {
	mapState       map[int]int    // state of map[filename]
	reduceState    map[int]int    // state of reduce[id]
	nReduce        int            // number of reduce as defined in the paper
	reduceFinished bool           // indicates if all reduce tasks done
	mapId2File     map[int]string //map mapID to filename
	mapCount       int            // map task count, equals number of files
	mutex          sync.Mutex     //atomicity for coordinator access
	workerCounter  int
}

// start a thread that listens for RPCs from worker.go
func (c *Coordinator) server() {
	rpc.Register(c)
	rpc.HandleHTTP()
	//l, e := net.Listen("tcp", ":1234")
	sockname := coordinatorSock()
	os.Remove(sockname)
	l, e := net.Listen("unix", sockname)
	if e != nil {
		log.Fatal("listen error:", e)
	}
	go http.Serve(l, nil)
}

// main/mrcoordinator.go calls Done() periodically to find out
// if the entire job has finished.
func (c *Coordinator) Done() bool {
	if c.reduceFinished {
		// wait same time for propagating the finished info to all workers
		time.Sleep(100 * time.Millisecond)
		return true
	}
	return false
}

/**
 * @description: worker call this via RPC to acquire task/report finished task
 * @param {*TaskRequest} args
 * @param {*TaskResponse} reply
 * @return {*}
 */
func (c *Coordinator) AllocateTasks(args *TaskRequest, reply *TaskResponse) error {
	workerId := args.WorkerId
	reply.NReduce = c.nReduce
	c.mutex.Lock()
	defer c.mutex.Unlock()
	// if all done, notify worker to exit
	if c.reduceFinished {
		reply.AllDone = true
		return nil
	}
	// if worker is idle
	if args.WorkerState == Idle {
		mapFinished := true
		for mapId, state := range c.mapState {
			if state != Finished {
				mapFinished = false
			}
			if state == UnAllocated {
				filename := c.mapId2File[mapId]
				// change state of the map task
				c.mapState[mapId] = Allocated
				// fill in reply to the worker
				reply.MapId = mapId + 1
				reply.TaskType = "map"
				reply.FileName = filename
				reply.NReduce = c.nReduce
				//record worker task in coordinator
				go func(mapID int, workerId int) {
					time.Sleep(10 * time.Second)
					c.mutex.Lock()
					if c.mapState[mapID] != Finished /*|| !fileExists(fmt.Sprintf("mr-%d-%d", mapID, 0)) */ {
						c.mapState[mapID] = UnAllocated
					}
					c.mutex.Unlock()
				}(mapId, workerId)
				return nil
			}
		}
		for reduceId, state := range c.reduceState {
			if !mapFinished {
				break
			}
			if state == UnAllocated {
				c.reduceState[reduceId] = Allocated
				reply.TaskType = "reduce"
				reply.ReduceId = reduceId + 1
				reply.MapCount = c.mapCount
				// notify the reduce worker with all the locations of
				// intermediate files
				go func(reduceId int, workerId int) {
					time.Sleep(10 * time.Second)
					c.mutex.Lock()
					if c.reduceState[reduceId] != Finished /*|| !fileExists(fmt.Sprintf("mr-out-%d.txt", reduceId)) */ {
						c.reduceState[reduceId] = UnAllocated
					}
					c.mutex.Unlock()
				}(reduceId, workerId)
				return nil
			}
		}
	} else if args.WorkerState == MapFinished {
		// label the specific map task as done
		c.mapState[args.MapId] = Finished
		// ask the worker to idle
		reply.TaskType = "idle"
	} else if args.WorkerState == ReduceFinished {
		c.reduceState[args.ReduceId] = Finished
		if checkReduceTaskAllDone(c) {
			c.reduceFinished = true
		}
		reply.TaskType = "idle"
	}
	return nil
}

func (c *Coordinator) RegisterWorker(args *RegisterArgs, reply *RegisterReply) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.workerCounter++
	workerId := c.workerCounter
	reply.WorkerId = workerId
	return nil
}

// create a Coordinator.
// main/mrcoordinator.go calls this function.
// nReduce is the number of reduce tasks to use.
func MakeCoordinator(files []string, nReduce int) *Coordinator {
	c := Coordinator{
		mapState:       make(map[int]int),
		reduceState:    make(map[int]int),
		nReduce:        nReduce,
		mapId2File:     make(map[int]string),
		reduceFinished: false,
		mutex:          sync.Mutex{},
	}
	// add files, create related map tasks
	for i, filename := range files {
		mapId := i
		c.mapCount++
		c.mapState[mapId] = UnAllocated
		c.mapId2File[mapId] = filename
	}
	// create reducer channel given NReduced
	for i := 0; i < nReduce; i++ {
		c.reduceState[i] = UnAllocated
	}
	c.server()
	return &c
}

// iterate all map tasks, chekc if all done
// func checkMapTaskAllDone(c *Coordinator) bool {
// 	// check state
// 	for _, state := range c.mapState {
// 		if state != Finished {
// 			return false
// 		}
// 	}
// 	// for robustness, check if map output files are present
// 	for i := 0; i < c.mapCount; i++ {
// 		if !fileExists(fmt.Sprintf("mr-%d-%d", i, 0)) {
// 			return false
// 		}
// 	}
// 	return true
// }

// iterate all reduce tasks, check if all done
func checkReduceTaskAllDone(c *Coordinator) bool {
	for _, state := range c.reduceState {
		if state != Finished {
			return false
		}
	}
	// for i := 0; i < c.nReduce; i++ {
	// 	if !fileExists(fmt.Sprintf("mr-out-%d.txt", i)) {
	// 		return false
	// 	}
	// }
	return true
}

func fileExists(filePath string) bool {
	_, err := os.Stat(filePath)
	return !os.IsNotExist(err)
}
