/*
 * @Author: amamiya-yuuko-1225 1913250675@qq.com
 * @Date: 2024-12-01 20:03:46
 * @LastEditors: amamiya-yuuko-1225 1913250675@qq.com
 * @Description:
 */
package mr

//
// RPC definitions.
//
// remember to capitalize all names.
//

import (
	"os"
	"strconv"
)

const (
	MapFinished = iota
	ReduceFinished
	Idle
)

type RegisterArgs struct {
	WorkerId int
}

type RegisterReply struct {
	WorkerId int
}

type TaskRequest struct {
	WorkerState int
	WorkerId    int
	MapId       int
	ReduceId    int
}

type TaskResponse struct {
	TaskType string
	FileName string
	ReduceId int
	MapId    int
	MapCount int
	NReduce  int
	AllDone  bool
}

// Add your RPC definitions here.

// Cook up a unique-ish UNIX-domain socket name
// in /var/tmp, for the coordinator.
// Can't use the current directory since
// Athena AFS doesn't support UNIX-domain sockets.
func coordinatorSock() string {
	s := "/var/tmp/5840-mr-"
	s += strconv.Itoa(os.Getuid())
	return s
}
