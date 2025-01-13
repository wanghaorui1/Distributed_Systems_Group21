package raft

//
// this is an outline of the API that raft must expose to
// the service (or tester). see comments below for
// each of these functions for more details.
//
// warning: `GetState` is not necessary.
//
// rf = Make(...)
//   create a new Raft server.
// rf.Start(command interface{}) (index, term, isleader)
//   start agreement on a new log entry
// rf.GetState() (term, isLeader)
//   ask a Raft for its current term, and whether it thinks it is leader
// ApplyMsg
//   each time a new entry is committed to the log, each Raft peer
//   should send an ApplyMsg to the service (or tester)
//   in the same order.
//

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"6.5840/labgob"
	"6.5840/labrpc"
)

// warning: the ticking granularity may acquire to be increased if there're more raft peers.
const tickInterval = 50 * time.Millisecond
const heartbeatTimeout = 150 * time.Millisecond
const None = -1 // to indicate a peer has not voted to anyone at the current term.
const baseElectionTimeout = 300

// true to turn on debugging/logging.
const debug = true
const LOGTOFILE = false
const printEnts = false

var ErrOutOfBound = errors.New("index out of bound")

type PeerState int

type RequestVoteArgs struct {
	From         int
	To           int
	Term         uint64
	LastLogIndex uint64
	LastLogTerm  uint64
}

type RequestVoteReply struct {
	From  int
	To    int
	Term  uint64
	Voted bool
}

type AppendEntriesArgs struct {
	From           int
	To             int
	Term           uint64
	CommittedIndex uint64
	PrevLogIndex   uint64
	PrevLogTerm    uint64
	Entries        []Entry
}

type Err int

const (
	Rejected Err = iota
	Matched
	IndexNotMatched
	TermNotMatched
)

type AppendEntriesReply struct {
	From               int
	To                 int
	Term               uint64
	Err                Err
	LastLogIndex       uint64
	ConflictTerm       uint64
	FirstConflictIndex uint64
}

type InstallSnapshotArgs struct {
	From     int
	To       int
	Term     uint64
	Snapshot Snapshot
}

type InstallSnapshotReply struct {
	From     int
	To       int
	Term     uint64
	CaughtUp bool
}

type MessageType string

const (
	Vote        MessageType = "RequestVote"
	VoteReply   MessageType = "RequestVoteReply"
	Append      MessageType = "AppendEntries"
	AppendReply MessageType = "AppendEntriesReply"
	Snap        MessageType = "InstallSnapshot"
	SnapReply   MessageType = "InstallSnapshotReply"
)

type Message struct {
	Type         MessageType
	From         int    // warning: not used for now.
	Term         uint64 // the term in the PRC args or RPC reply.
	ArgsTerm     uint64 // the term in the RPC args. Used to different between the term in a RPC reply.
	PrevLogIndex uint64 // used for checking of AppendEntriesReply.
}

type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int

	SnapshotValid bool
	Snapshot      []byte
	SnapshotIndex int
	SnapshotTerm  int
}

const (
	Follower PeerState = iota
	Candidate
	Leader
)

// A Go object implementing a single Raft peer.
type Raft struct {
	mu        sync.Mutex
	peers     []*labrpc.ClientEnd // RPC end points of all peers
	persister *Persister          // Object to hold this peer's persisted state
	me        int                 // this peer's index into peers[]
	dead      int32               // set by Kill()

	state   PeerState
	term    uint64
	votedTo int
	votedMe []bool // true if a peer has voted to me at the current election.

	electionTimeout time.Duration
	lastElection    time.Time

	heartbeatTimeout time.Duration
	lastHeartbeat    time.Time

	log Log

	peerTrackers []PeerTracker // keeps track of each peer's next index, match index, etc.

	applyCh          chan<- ApplyMsg
	claimToBeApplied sync.Cond

	logger Logger
}

type Entry struct {
	Index uint64
	Term  uint64
	Data  interface{}
}

type Snapshot struct {
	Data  []byte
	Index uint64
	Term  uint64
}

// if the peer has not acked in this duration, it's considered inactive.
const activeWindowWidth = 2 * baseElectionTimeout * time.Millisecond

type PeerTracker struct {
	nextIndex  uint64
	matchIndex uint64

	lastAck time.Time
}

func Make(peers []*labrpc.ClientEnd, me int,
	persister *Persister, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{}
	rf.mu = sync.Mutex{}
	rf.peers = peers
	rf.persister = persister
	rf.me = me

	rf.logger = *makeLogger(false, "out")
	rf.logger.r = rf

	rf.applyCh = applyCh
	rf.claimToBeApplied = *sync.NewCond(&rf.mu)

	rf.log = makeLog()
	rf.log.logger = &rf.logger

	if rf.persister.RaftStateSize() > 0 {
		rf.readPersist(rf.persister.ReadRaftState())

	} else {
		rf.term = 0
		rf.votedTo = None
	}

	// update tracked indexes with the restored log entries.
	rf.peerTrackers = make([]PeerTracker, len(rf.peers))
	rf.resetTrackedIndexes()

	rf.state = Follower
	rf.resetElectionTimer()
	rf.heartbeatTimeout = heartbeatTimeout

	go rf.ticker()
	go rf.committer()

	return rf
}

func (rf *Raft) ticker() {
	for !rf.killed() {
		rf.mu.Lock()

		switch rf.state {
		case Follower:
			fallthrough
		case Candidate:
			if rf.pastElectionTimeout() {
				rf.becomeCandidate()
				rf.broadcastRequestVote()
			}

		case Leader:
			if !rf.quorumActive() {
				rf.becomeFollower(rf.term)
				break
			}

			forced := false
			if rf.pastHeartbeatTimeout() {
				forced = true
				rf.resetHeartbeatTimer()
			}
			rf.broadcastAppendEntries(forced)
		}

		rf.mu.Unlock()
		time.Sleep(tickInterval)
	}
}

func (rf *Raft) Start(command interface{}) (int, int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	// warning: the `rf.killed` checking is not necessary.
	isLeader := !rf.killed() && rf.state == Leader
	if !isLeader {
		return 0, 0, false
	}

	index := rf.log.lastIndex() + 1
	entry := Entry{Index: index, Term: rf.term, Data: command}
	rf.log.append([]Entry{entry})
	rf.persist()

	rf.broadcastAppendEntries(true)

	// warning: the returned index and term are only used by tester and logger.
	return int(index), int(rf.term), true
}

func (rf *Raft) GetState() (int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	// warning: the `rf.killed` checking is not necessary.
	return int(rf.term), !rf.killed() && rf.state == Leader
}

func (rf *Raft) Kill() {
	atomic.StoreInt32(&rf.dead, 1)
}

func (rf *Raft) killed() bool {
	z := atomic.LoadInt32(&rf.dead)
	return z == 1
}

func (rf *Raft) committer() {
	rf.mu.Lock()
	for !rf.killed() {
		if rf.log.hasPendingSnapshot {
			snapshot := rf.log.clonedSnapshot()
			rf.mu.Unlock()

			rf.applyCh <- ApplyMsg{SnapshotValid: true, Snapshot: snapshot.Data, SnapshotIndex: int(snapshot.Index), SnapshotTerm: int(snapshot.Term)}

			rf.mu.Lock()
			rf.log.hasPendingSnapshot = false

		} else if newCommittedEntries := rf.log.newCommittedEntries(); len(newCommittedEntries) > 0 {
			rf.mu.Unlock()

			for _, entry := range newCommittedEntries {
				rf.applyCh <- ApplyMsg{CommandValid: true, Command: entry.Data, CommandIndex: int(entry.Index)}
			}

			rf.mu.Lock()
			rf.log.appliedTo(newCommittedEntries[len(newCommittedEntries)-1].Index)

		} else {
			rf.claimToBeApplied.Wait()
		}
	}
	rf.mu.Unlock()
}

func (rf *Raft) pastElectionTimeout() bool {
	return time.Since(rf.lastElection) > rf.electionTimeout
}

func (rf *Raft) resetElectionTimer() {
	electionTimeout := baseElectionTimeout + (rand.Int63() % baseElectionTimeout)
	rf.electionTimeout = time.Duration(electionTimeout) * time.Millisecond
	rf.lastElection = time.Now()
}

func (rf *Raft) becomeFollower(term uint64) bool {
	rf.state = Follower
	if term > rf.term {
		rf.term = term
		rf.votedTo = None
		return true
	}
	return false
}

func (rf *Raft) becomeCandidate() {
	defer rf.persist()
	rf.state = Candidate
	rf.term++
	rf.votedMe = make([]bool, len(rf.peers))
	rf.votedTo = rf.me
	rf.resetElectionTimer()
}

func (rf *Raft) becomeLeader() {
	rf.state = Leader
	rf.resetTrackedIndexes()
}

func (rf *Raft) makeRequestVoteArgs(to int) *RequestVoteArgs {
	lastLogIndex := rf.log.lastIndex()
	lastLogTerm, _ := rf.log.term(lastLogIndex)
	args := &RequestVoteArgs{From: rf.me, To: to, Term: rf.term, LastLogIndex: lastLogIndex, LastLogTerm: lastLogTerm}
	return args
}

func (rf *Raft) sendRequestVote(args *RequestVoteArgs) {
	reply := RequestVoteReply{}
	// note: `Call` has an internal timeout mechanism.
	if ok := rf.peers[args.To].Call("Raft.RequestVote", args, &reply); ok {
		rf.handleRequestVoteReply(args, &reply)
	}
}

func (rf *Raft) broadcastRequestVote() {
	for i := range rf.peers {
		if i != rf.me {
			args := rf.makeRequestVoteArgs(i)
			go rf.sendRequestVote(args)
		}
	}
}

func (rf *Raft) eligibleToGrantVote(candidateLastLogIndex, candidateLastLogTerm uint64) bool {
	lastLogIndex := rf.log.lastIndex()
	lastLogTerm, _ := rf.log.term(lastLogIndex)
	return candidateLastLogTerm > lastLogTerm || (candidateLastLogTerm == lastLogTerm && candidateLastLogIndex >= lastLogIndex)
}

func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	reply.From = rf.me
	reply.To = args.From
	reply.Term = rf.term
	reply.Voted = false

	m := Message{Type: Vote, From: args.From, Term: args.Term}
	ok, termChanged := rf.checkMessage(m)
	if termChanged {
		reply.Term = rf.term
		defer rf.persist()
	}
	if !ok {
		return
	}

	if (rf.votedTo == None || rf.votedTo == args.From) && rf.eligibleToGrantVote(args.LastLogIndex, args.LastLogTerm) {
		rf.votedTo = args.From
		rf.resetElectionTimer()
		reply.Voted = true
	}
}

func (rf *Raft) quorumVoted() bool {
	votes := 1
	for i, votedMe := range rf.votedMe {
		if i != rf.me && votedMe {
			votes++
		}
	}
	return 2*votes > len(rf.peers)
}

func (rf *Raft) handleRequestVoteReply(args *RequestVoteArgs, reply *RequestVoteReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	m := Message{Type: VoteReply, From: reply.From, Term: reply.Term, ArgsTerm: args.Term}
	ok, termChanged := rf.checkMessage(m)
	if termChanged {
		defer rf.persist()
	}
	if !ok {
		return
	}

	if reply.Voted {
		rf.votedMe[reply.From] = true
		if rf.quorumVoted() {
			rf.becomeLeader()
		}
	}
}

// Log manages log entries, its struct look like:
//
//	     snapshot/first.....applied....committed.....last
//	-------------|--------------------------------------|
//	  compacted           persisted log entries
type Log struct {
	// compacted log entries.
	snapshot           Snapshot
	hasPendingSnapshot bool // true if the snapshot is not yet delivered to the application.

	// persisted log entries.
	entries []Entry

	// TODO: rename applied with delivered. Update comments and docs as well.
	applied   uint64 // the highest log index of the log entry raft knows that the application has applied.
	committed uint64 // the highest log index of the log entry raft knows that the raft cluster has committed.

	logger *Logger
}

func makeLog() Log {
	log := Log{
		snapshot:           Snapshot{Data: nil, Index: 0, Term: 0},
		hasPendingSnapshot: false,
		entries:            []Entry{{Index: 0, Term: 0}}, // use a dummy entry to simplify indexing operations.
		applied:            0,
		committed:          0,
	}

	return log
}

func (log *Log) toArrayIndex(index uint64) uint64 {
	// warning: an unstable implementation may incur integer underflow. (my implementation is stable now)
	return index - log.firstIndex()
}

// always return the snapshot index.
func (log *Log) firstIndex() uint64 {
	return log.entries[0].Index
}

func (log *Log) lastIndex() uint64 {
	return log.entries[len(log.entries)-1].Index
}

func (log *Log) term(index uint64) (uint64, error) {
	if index < log.firstIndex() || index > log.lastIndex() {
		return 0, ErrOutOfBound
	}
	index = log.toArrayIndex(index)
	return log.entries[index].Term, nil
}

func (log *Log) clone(entries []Entry) []Entry {
	cloned := make([]Entry, len(entries))
	copy(cloned, entries)
	return cloned
}

func (log *Log) slice(start, end uint64) []Entry {
	if start == end {
		// can only happen when sending a heartbeat.
		return nil
	}
	start = log.toArrayIndex(start)
	end = log.toArrayIndex(end)
	return log.clone(log.entries[start:end])
}

// FIXME: doubt the out of bound checking is necessary.
// seems only the `index > log.lastIndex()` checking is necessary.
func (log *Log) truncateSuffix(index uint64) {
	if index <= log.firstIndex() || index > log.lastIndex() {
		return
	}

	index = log.toArrayIndex(index)
	if len(log.entries[index:]) > 0 {
		log.entries = log.entries[:index]
	}
}

func (log *Log) append(entries []Entry) {
	log.entries = append(log.entries, entries...)
}

func (log *Log) committedTo(index uint64) {
	if index > log.committed {
		log.committed = index
	}
}

func (log *Log) newCommittedEntries() []Entry {
	start := log.toArrayIndex(log.applied + 1)
	end := log.toArrayIndex(log.committed + 1)
	// FIXME: replace with `start == end` and verify it.
	if start >= end {
		// note: len(nil slice) == 0.
		return nil
	}
	return log.clone(log.entries[start:end])
}

func (log *Log) appliedTo(index uint64) {
	// FIXME: doubt the checking is necessary.
	if index > log.applied {
		log.applied = index
	}
}

func (log *Log) compactedTo(snapshot Snapshot) {
	suffix := make([]Entry, 0)
	suffixStart := snapshot.Index + 1
	if suffixStart <= log.lastIndex() {
		suffixStart = log.toArrayIndex(suffixStart)
		suffix = log.entries[suffixStart:]
	}

	log.entries = append(make([]Entry, 1), suffix...)
	log.snapshot = snapshot
	// set the dummy entry.
	log.entries[0] = Entry{Index: snapshot.Index, Term: snapshot.Term}

	log.committedTo(log.snapshot.Index)
	log.appliedTo(log.snapshot.Index)
}

// FIXME: doubt the clone is necessary for working around races.
func (log *Log) clonedSnapshot() Snapshot {
	cloned := Snapshot{Data: make([]byte, len(log.snapshot.Data)), Index: log.snapshot.Index, Term: log.snapshot.Term}
	copy(cloned.Data, log.snapshot.Data)
	return cloned
}

// the service says it has created a snapshot that has
// all info up to and including index. this means the
// service no longer needs the log through (and including)
// that index. Raft should now trim its log as much as possible.
func (rf *Raft) Snapshot(index int, snapshot []byte) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	// it's possible there's a pending snapshot received from the leader that is not delivered yet to the
	// server. The server may meanwhile checkpoint at a lower snapshot index which may produce a stale snapshot.
	snapshotIndex := uint64(index)
	if snapshotIndex <= rf.log.snapshot.Index {
		return
	}

	snapshotTerm, _ := rf.log.term(snapshotIndex)
	rf.log.compactedTo(Snapshot{Data: snapshot, Index: snapshotIndex, Term: snapshotTerm})
	rf.persist()
}

func (rf *Raft) makeInstallSnapshotArgs(to int) *InstallSnapshotArgs {
	args := &InstallSnapshotArgs{From: rf.me, To: to, Term: rf.term, Snapshot: rf.log.clonedSnapshot()}
	return args
}

func (rf *Raft) sendInstallSnapshot(args *InstallSnapshotArgs) {
	reply := InstallSnapshotReply{}
	if ok := rf.peers[args.To].Call("Raft.InstallSnapshot", args, &reply); ok {
		rf.handleInstallSnapshotReply(args, &reply)
	}
}

func (rf *Raft) lagBehindSnapshot(to int) bool {
	return rf.peerTrackers[to].nextIndex <= rf.log.firstIndex()
}

func (rf *Raft) InstallSnapshot(args *InstallSnapshotArgs, reply *InstallSnapshotReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	reply.From = rf.me
	reply.To = args.From
	reply.Term = rf.term
	reply.CaughtUp = false

	m := Message{Type: Snap, From: args.From, Term: args.Term}
	ok, termChanged := rf.checkMessage(m)
	if termChanged {
		reply.Term = rf.term
		defer rf.persist()
	}
	if !ok {
		return
	}

	// reject the snapshot if this peer has already caught up.
	if args.Snapshot.Index <= rf.log.committed {
		// but return `CaughtUp` true to handle unreliable network, e.g. discard, dup.
		reply.CaughtUp = true
		return
	}

	rf.log.compactedTo(args.Snapshot)
	reply.CaughtUp = true
	if !termChanged {
		defer rf.persist()
	}

	rf.log.hasPendingSnapshot = true
	rf.claimToBeApplied.Signal()
}

func (rf *Raft) handleInstallSnapshotReply(args *InstallSnapshotArgs, reply *InstallSnapshotReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	m := Message{Type: SnapReply, From: reply.From, Term: reply.Term, ArgsTerm: args.Term}
	ok, termChanged := rf.checkMessage(m)
	if termChanged {
		defer rf.persist()
	}
	if !ok {
		return
	}

	if reply.CaughtUp {
		rf.peerTrackers[reply.From].matchIndex = args.Snapshot.Index
		rf.peerTrackers[reply.From].nextIndex = rf.peerTrackers[reply.From].matchIndex + 1

		// note: there must already have a majority of followers whose match index is greater than or equal to
		// the snapshot index. Otherwise, the snapshot won't be generated.
		// hence, the update of the match index of a lag-behind follower won't drive the update
		// of the committed index. Hence, there's no need to call `maybeCommitMatched`.
		// the forcing broadcast AppendEntries is used to make lag-behind followers catch up quickly.
		rf.broadcastAppendEntries(true)
	}
}

func (rf *Raft) pastHeartbeatTimeout() bool {
	return time.Since(rf.lastHeartbeat) > rf.heartbeatTimeout
}

func (rf *Raft) resetHeartbeatTimer() {
	rf.lastHeartbeat = time.Now()
}

func (rf *Raft) makeAppendEntriesArgs(to int) *AppendEntriesArgs {
	nextIndex := rf.peerTrackers[to].nextIndex
	entries := rf.log.slice(nextIndex, rf.log.lastIndex()+1)

	prevLogIndex := nextIndex - 1
	prevLogTerm, _ := rf.log.term(prevLogIndex)

	args := &AppendEntriesArgs{From: rf.me, To: to, Term: rf.term, CommittedIndex: rf.log.committed, PrevLogIndex: prevLogIndex, PrevLogTerm: prevLogTerm, Entries: entries}
	return args
}

func (rf *Raft) sendAppendEntries(args *AppendEntriesArgs) {
	reply := AppendEntriesReply{}
	if ok := rf.peers[args.To].Call("Raft.AppendEntries", args, &reply); ok {
		rf.handleAppendEntriesReply(args, &reply)
	}
}

func (rf *Raft) hasNewEntries(to int) bool {
	return rf.log.lastIndex() >= rf.peerTrackers[to].nextIndex
}

func (rf *Raft) broadcastAppendEntries(forced bool) {
	for i := range rf.peers {
		if i == rf.me {
			continue
		}

		if rf.lagBehindSnapshot(i) {
			args := rf.makeInstallSnapshotArgs(i)
			go rf.sendInstallSnapshot(args)

		} else if forced || rf.hasNewEntries(i) {
			args := rf.makeAppendEntriesArgs(i)
			go rf.sendAppendEntries(args)
		}
	}
}

func (rf *Raft) checkLogPrefixMatched(leaderPrevLogIndex, leaderPrevLogTerm uint64) Err {
	prevLogTerm, err := rf.log.term(leaderPrevLogIndex)
	if err != nil {
		return IndexNotMatched
	}
	if prevLogTerm != leaderPrevLogTerm {
		return TermNotMatched
	}
	return Matched
}

func (rf *Raft) findFirstConflict(index uint64) (uint64, uint64) {
	conflictTerm, _ := rf.log.term(index)
	firstConflictIndex := index
	// warning: skip the snapshot index since it cannot conflict if all goes well.
	for i := index - 1; i > rf.log.firstIndex(); i-- {
		if term, _ := rf.log.term(i); term != conflictTerm {
			break
		}
		firstConflictIndex = i
	}
	return conflictTerm, firstConflictIndex
}

func (rf *Raft) maybeCommittedTo(index uint64) {
	if index > rf.log.committed {
		rf.log.committedTo(index)
		rf.claimToBeApplied.Signal()
	}
}

func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	reply.From = rf.me
	reply.To = args.From
	reply.Term = rf.term
	reply.Err = Rejected

	m := Message{Type: Append, From: args.From, Term: args.Term}
	ok, termChanged := rf.checkMessage(m)
	if termChanged {
		reply.Term = rf.term
		defer rf.persist()
	}
	if !ok {
		return
	}

	reply.Err = rf.checkLogPrefixMatched(args.PrevLogIndex, args.PrevLogTerm)
	if reply.Err != Matched {
		if reply.Err == IndexNotMatched {
			reply.LastLogIndex = rf.log.lastIndex()
		} else {
			reply.ConflictTerm, reply.FirstConflictIndex = rf.findFirstConflict(args.PrevLogIndex)
		}
		return
	}

	for i, entry := range args.Entries {
		if term, err := rf.log.term(entry.Index); err != nil || term != entry.Term {
			rf.log.truncateSuffix(entry.Index)
			rf.log.append(args.Entries[i:])
			if !termChanged {
				rf.persist()
			}
			break
		}
	}

	lastNewLogIndex := min(args.CommittedIndex, args.PrevLogIndex+uint64(len(args.Entries)))
	rf.maybeCommittedTo(lastNewLogIndex)
}

func (rf *Raft) quorumMatched(index uint64) bool {
	matched := 1
	for _, tracker := range rf.peerTrackers {
		if tracker.matchIndex >= index {
			matched++
		}
	}
	return 2*matched > len(rf.peers)
}

func (rf *Raft) maybeCommitMatched(index uint64) bool {
	for i := index; i > rf.log.committed; i-- {
		if term, _ := rf.log.term(i); term == rf.term && rf.quorumMatched(i) {
			rf.log.committedTo(i)
			rf.claimToBeApplied.Signal()
			return true
		}
	}
	return false
}

func (rf *Raft) handleAppendEntriesReply(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	m := Message{Type: AppendReply, From: reply.From, Term: reply.Term, ArgsTerm: args.Term, PrevLogIndex: args.PrevLogIndex}
	ok, termChanged := rf.checkMessage(m)
	if termChanged {
		defer rf.persist()
	}
	if !ok {
		return
	}

	switch reply.Err {
	case Rejected:
		// do nothing.

	case Matched:
		rf.peerTrackers[reply.From].matchIndex = args.PrevLogIndex + uint64(len(args.Entries))
		rf.peerTrackers[reply.From].nextIndex = rf.peerTrackers[reply.From].matchIndex + 1

		// broadcast immediately if the committed index got updated so that follower can
		// learn the committed index sooner.
		if rf.maybeCommitMatched(rf.peerTrackers[reply.From].matchIndex) {
			rf.broadcastAppendEntries(true)
		}

	case IndexNotMatched:
		// warning: only if the follower's log is actually shorter than the leader's,
		// the leader could adopt the follower's last log index.
		// in any cases, the next index cannot be larger than the leader's last log index + 1.
		if reply.LastLogIndex < rf.log.lastIndex() {
			rf.peerTrackers[reply.From].nextIndex = reply.LastLogIndex + 1
		} else {
			rf.peerTrackers[reply.From].nextIndex = rf.log.lastIndex() + 1
		}

		// broadcast immediately so that followers can quickly catch up.
		rf.broadcastAppendEntries(true)

	case TermNotMatched:
		newNextIndex := reply.FirstConflictIndex
		// warning: skip the snapshot index since it cannot conflict if all goes well.
		for i := rf.log.lastIndex(); i > rf.log.firstIndex(); i-- {
			if term, _ := rf.log.term(i); term == reply.ConflictTerm {
				newNextIndex = i
				break
			}
		}

		// FIXME: figure out whether the next index is eligible to be advanced.
		rf.peerTrackers[reply.From].nextIndex = newNextIndex

		// broadcast immediately so that followers can quickly catch up.
		rf.broadcastAppendEntries(true)
	}
}

func (l *Logger) printEnts(topic logTopic, me int, ents []Entry) {
	if printEnts {
		for _, ent := range ents {
			if ent.Index != 0 {
				l.printf(topic, "N%v    (I:%v T:%v D:%v)", me, ent.Index, ent.Term, ent.Data.(int))
				// l.printf(topic, "N%v    (I:%v T:%v)", me, ent.Index, ent.Term)
			}
		}
	}
}

// what topic the log message is related to.
// logs are organized by topics which further consists of events.
type logTopic string

const (
	ELEC logTopic = "ELEC"
	LRPE logTopic = "LRPE"
	BEAT logTopic = "BEAT"
	PERS logTopic = "PERS"
	PEER logTopic = "PEER"
	SNAP logTopic = "SNAP"
)

type Logger struct {
	logToFile      bool
	logFile        *os.File
	verbosityLevel int // logging verbosity is controlled over environment verbosity variable.
	startTime      time.Time
	r              *Raft
}

func makeLogger(logToFile bool, logFileName string) *Logger {
	logger := &Logger{}
	logger.init(LOGTOFILE, logFileName)
	return logger
}

func (logger *Logger) init(logToFile bool, logFileName string) {
	logger.logToFile = logToFile
	logger.verbosityLevel = getVerbosityLevel()
	logger.startTime = time.Now()

	// set log config.
	if logger.logToFile {
		logger.setLogFile(logFileName)
	}
	log.SetFlags(log.Flags() & ^(log.Ldate | log.Ltime)) // not show date and time.
}

func (logger *Logger) setLogFile(filename string) {
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		log.Fatalf("failed to create file %v", filename)
	}
	log.SetOutput(f)
	logger.logFile = f
}

func (logger *Logger) printf(topic logTopic, format string, a ...interface{}) {
	// print iff debug is set.
	if debug {
		// time := time.Since(logger.startTime).Milliseconds()
		time := time.Since(logger.startTime).Microseconds()
		// e.g. 008256 VOTE ...
		prefix := fmt.Sprintf("%010d %v ", time, string(topic))
		format = prefix + format
		log.Printf(format, a...)
	}
}

// not delete this for backward compatibility.
func DPrintf(format string, a ...interface{}) (n int, err error) {
	if debug {
		log.Printf(format, a...)
	}
	return
}

// retrieve the verbosity level from an environment variable
// VERBOSE=0/1/2/3 <=>
func getVerbosityLevel() int {
	v := os.Getenv("VERBOSE")
	level := 0
	if v != "" {
		var err error
		level, err = strconv.Atoi(v)
		if err != nil {
			log.Fatalf("Invalid verbosity %v", v)
		}
	}
	return level
}

//
// leader election events.
//

var stmap = [...]string{
	"F", // follower
	"C", // candidate
	"L", // leader
}

func (st PeerState) String() string {
	return stmap[uint64(st)]
}

func (l *Logger) elecTimeout() {
	r := l.r
	l.printf(ELEC, "N%v ETO (S:%v T:%v)", r.me, r.state, r.term)
}

func (l *Logger) stepDown() {
	r := l.r
	l.printf(ELEC, "N%v STD (T:%v)", r.me, r.term)
}

func (l *Logger) stateToCandidate() {
	r := l.r
	l.printf(ELEC, "N%v %v->%v (T:%v)", r.me, r.state, Candidate, r.term)
}

func (l *Logger) bcastRVOT() {
	r := l.r
	l.printf(ELEC, "N%v @ RVOT (T:%v)", r.me, r.term)
}

func (l *Logger) recvRVOT(m *RequestVoteArgs) {
	r := l.r
	l.printf(ELEC, "N%v <- N%v RVOT (T:%v)", r.me, m.From, m.Term)
}

func (l *Logger) voteTo(to int) {
	r := l.r
	l.printf(ELEC, "N%v v-> N%v", r.me, to)
}

var denyReasonMap = [...]string{
	"GRT", // grant the vote.
	"VTD", // I've granted the vote to another one.
	"STL", // you're stale.
}

func (l *Logger) rejectVoteTo(to int, CandidatelastLogIndex, CandidatelastLogTerm, lastLogIndex, lastLogTerm uint64) {
	r := l.r
	l.printf(ELEC, "N%v !v-> N%v (CLI:%v CLT:%v LI:%v LT:%v)", r.me, to,
		CandidatelastLogIndex, CandidatelastLogTerm, lastLogIndex, lastLogTerm)
}

func (l *Logger) recvRVOTRes(m *RequestVoteReply) {
	r := l.r
	l.printf(ELEC, "N%v <- N%v RVOT RES (T:%v V:%v)", r.me, m.From, m.Term, m.Voted)
}

func (l *Logger) recvVoteQuorum() {
	r := l.r
	l.printf(ELEC, "N%v <- VOTE QUORUM (T:%v)", r.me, r.term)
}

func (l *Logger) stateToLeader() {
	r := l.r
	l.printf(ELEC, "N%v %v->%v (T:%v)", r.me, r.state, Leader, r.term)
}

func (l *Logger) stateToFollower(oldTerm uint64) {
	r := l.r
	l.printf(ELEC, "N%v %v->%v (T:%v) -> (T:%v)", r.me, r.state, Follower, oldTerm, r.term)
}

//
// log replication events.
//

func (l *Logger) appendEnts(ents []Entry) {
	r := l.r
	l.printf(LRPE, "N%v +e (LN:%v)", r.me, len(ents))
}

func (l *Logger) sendEnts(prevLogIndex, prevLogTerm uint64, ents []Entry, to int) {
	r := l.r
	l.printf(LRPE, "N%v e-> N%v (T:%v CI:%v PI:%v PT:%v LN:%v)", r.me, to, r.term, r.log.committed, prevLogIndex, prevLogTerm, len(ents))
	l.printEnts(LRPE, r.me, ents)
}

func (l *Logger) recvAENT(m *AppendEntriesArgs) {
	r := l.r
	l.printf(LRPE, "N%v <- N%v AENT (T:%v CI:%v PI:%v PT:%v LN:%v)", r.me, m.From, m.Term, m.CommittedIndex, m.PrevLogIndex, m.PrevLogTerm, len(m.Entries))
}

type RejectReason int

const (
	Accepted RejectReason = iota
	IndexConflict
	TermConflict
)

var reasonMap = [...]string{
	"NO", // not reject
	"IC", // index conflict.
	"TC", // term conflict.
}

func (l *Logger) rejectEnts(from int) {
	r := l.r
	l.printf(LRPE, "N%v !e<- N%v", r.me, from)
}

func (l *Logger) acceptEnts(from int) {
	r := l.r
	l.printf(LRPE, "N%v &e<- N%v", r.me, from)
}

func (l *Logger) discardEnts(ents []Entry) {
	r := l.r
	l.printf(LRPE, "N%v -e (LN:%v)", r.me, len(ents))
	l.printEnts(LRPE, r.me, ents)
}

var errMap = [...]string{
	"RJ", // rejected.
	"MT", // matched.
	"IN", // index not matched.
	"TN", // term not matched.
}

func (err Err) String() string {
	return errMap[err]
}

func (l *Logger) recvAENTRes(m *AppendEntriesReply) {
	r := l.r
	l.printf(LRPE, "N%v <- N%v AENT RES (T:%v E:%v CT:%v FCI:%v LI:%v)", r.me, m.From, m.Term, errMap[m.Err], m.ConflictTerm, m.FirstConflictIndex, m.LastLogIndex)
}

func (l *Logger) updateProgOf(peer int, oldNext, oldMatch, newNext, newMatch uint64) {
	r := l.r
	l.printf(LRPE, "N%v ^pr N%v (NI:%v MI:%v) -> (NI:%v MI:%v)", r.me, peer, oldNext, oldMatch, newNext, newMatch)
}

func (l *Logger) updateCommitted(oldCommitted uint64) {
	r := l.r
	l.printf(LRPE, "N%v ^ci (CI:%v) -> (CI:%v)", r.me, oldCommitted, r.log.committed)
}

func (l *Logger) updateApplied(oldApplied uint64) {
	r := l.r
	l.printf(LRPE, "N%v ^ai (AI:%v) -> (AI:%v)", r.me, oldApplied, r.log.applied)
}

//
// heartbeat events.
//

func (l *Logger) sendBeat(prevLogIndex, prevLogTerm uint64, to int) {
	r := l.r
	l.printf(LRPE, "N%v b-> N%v (T:%v CI:%v PI:%v PT:%v)", r.me, to, r.term, r.log.committed, prevLogIndex, prevLogTerm)
}

func (l *Logger) recvHBET(m *AppendEntriesArgs) {
	r := l.r
	l.printf(BEAT, "N%v <- N%v HBET (T:%v CI:%v)", r.me, m.From, m.Term, m.CommittedIndex)
}

func (l *Logger) recvHBETRes(m *AppendEntriesReply) {
	r := l.r
	l.printf(LRPE, "N%v <- N%v HBET RES (T:%v E:%v CT:%v FCI:%v LI:%v)", r.me, m.From, m.Term, errMap[m.Err], m.ConflictTerm, m.FirstConflictIndex, m.LastLogIndex)
}

//
// persistence events.
//

func (l *Logger) restore() {
	r := l.r
	l.printf(PERS, "N%v rs (T:%v V:%v LI:%v CI:%v AI:%v SI:%v ST:%v)", r.me, r.term, r.votedTo, r.log.lastIndex(), r.log.committed, r.log.applied, r.log.snapshot.Index, r.log.snapshot.Term)
	if printEnts {
		l.printEnts(PERS, r.me, r.log.entries)
	}
}

func (l *Logger) persist() {
	r := l.r
	l.printf(PERS, "N%v sv (T:%v V:%v LI:%v CI:%v AI:%v SI:%v ST:%v)", r.me, r.term, r.votedTo, r.log.lastIndex(), r.log.committed, r.log.applied, r.log.snapshot.Index, r.log.snapshot.Term)
	if printEnts {
		l.printEnts(PERS, r.me, r.log.entries)
	}
}

//
// snapshot events
//

func (l *Logger) compactedTo(lastLogIndex, lastLogTerm uint64) {
	r := l.r
	l.printf(SNAP, "N%v cp (SI:%v ST:%v LI:%v LT:%v)", r.me, r.log.snapshot.Index, r.log.snapshot.Term, lastLogIndex, lastLogTerm)
}

func (l *Logger) sendISNP(to int, snapshotIndex, snapshotTerm uint64) {
	r := l.r
	l.printf(SNAP, "N%v s-> N%v (SI:%v ST:%v)", r.me, to, snapshotIndex, snapshotTerm)
}

func (l *Logger) recvISNP(m *InstallSnapshotArgs) {
	r := l.r
	l.printf(SNAP, "N%v <- N%v ISNP (SI:%v ST:%v)", r.me, m.From, m.Snapshot.Index, m.Snapshot.Term)
}

func (l *Logger) recvISNPRes(m *InstallSnapshotReply) {
	r := l.r
	l.printf(SNAP, "N%v <- N%v ISNP RES (IS:%v)", r.me, m.From, m.CaughtUp)
}

func (l *Logger) pullSnap(snapshotIndex uint64) {
	r := l.r
	l.printf(SNAP, "N%v pull SNP (SI:%v)", r.me, snapshotIndex)
}

func (l *Logger) pushSnap(snapshotIndex, snapshotTerm uint64) {
	r := l.r
	l.printf(SNAP, "N%v push SNP (SI:%v ST:%v)", r.me, snapshotIndex, snapshotTerm)
}

// return (termIsStale, termChanged).
func (rf *Raft) checkTerm(m Message) (bool, bool) {
	// ignore stale messages.
	if m.Term < rf.term {
		return false, false
	}
	// step down if received a more up-to-date message or received a message from the current leader.
	if m.Term > rf.term || (m.Type == Append || m.Type == Snap) {
		termChanged := rf.becomeFollower(m.Term)
		return true, termChanged
	}
	return true, false
}

// return true if the raft peer is eligible to handle the message.
func (rf *Raft) checkState(m Message) bool {
	eligible := false

	switch m.Type {
	// only a follower is eligible to handle RequestVote, AppendEntries, and InstallSnapshot.
	case Vote:
		fallthrough
	case Append:
		eligible = rf.state == Follower
	case Snap:
		// warning: not rejecting a new snapshot if there's a pending snapshot may shadow the new snapshot.
		eligible = rf.state == Follower && !rf.log.hasPendingSnapshot

	case VoteReply:
		// `rf.term == m.Term` ensures that the sender is in the same term as when sending the message.
		eligible = rf.state == Candidate && rf.term == m.ArgsTerm
	case AppendReply:
		// the checking of next index ensures it's exactly the reply corresponding to the last sent AppendEntries.
		eligible = rf.state == Leader && rf.term == m.ArgsTerm && rf.peerTrackers[m.From].nextIndex-1 == m.PrevLogIndex
	case SnapReply:
		// the lag-behind checking ensures the reply corresponds to the last sent InstallSnapshot.
		eligible = rf.state == Leader && rf.term == m.ArgsTerm && rf.lagBehindSnapshot(m.From)

	default:
		log.Fatalf("unexpected message type %v", m.Type)
	}

	// a follower is recommended to reset the election timer to not compete with the acknowledged leader.
	// warning: it's only recommended, not mandatory.
	if rf.state == Follower && (m.Type == Append || m.Type == Snap) {
		rf.resetElectionTimer()
	}

	return eligible
}

func (rf *Raft) checkMessage(m Message) (bool, bool) {
	// refresh the step down timer if received a reply.
	// warning: no need to differentiate peer state.
	if m.Type == VoteReply || m.Type == AppendReply || m.Type == SnapReply {
		rf.peerTrackers[m.From].lastAck = time.Now()
	}

	ok, termChanged := rf.checkTerm(m)
	if !ok || !rf.checkState(m) {
		return false, termChanged
	}
	return true, termChanged
}

func (rf *Raft) persist() {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	if e.Encode(rf.term) != nil || e.Encode(rf.votedTo) != nil || e.Encode(rf.log.entries) != nil || e.Encode(rf.log.snapshot.Index) != nil || e.Encode(rf.log.snapshot.Term) != nil {
		panic("failed to encode some fields")
	}

	raftstate := w.Bytes()
	// warning: since the persister provides a very simple interface, there's no way to not persist
	// raftstate and snapshot together while ensures they're in sync.
	rf.persister.Save(raftstate, rf.log.snapshot.Data)
}

func (rf *Raft) readPersist(data []byte) {
	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)
	if d.Decode(&rf.term) != nil || d.Decode(&rf.votedTo) != nil || d.Decode(&rf.log.entries) != nil || d.Decode(&rf.log.snapshot.Index) != nil || d.Decode(&rf.log.snapshot.Term) != nil {
		panic("failed to decode some fields")
	}

	// warning: on recovery, raft has to also restore the snapshot.
	// that's because a leader might need to send a snapshot to followers after restarted.
	rf.log.compactedTo(Snapshot{Data: rf.persister.ReadSnapshot(), Index: rf.log.snapshot.Index, Term: rf.log.snapshot.Term})
}

func (rf *Raft) resetTrackedIndexes() {
	for i := range rf.peerTrackers {
		rf.peerTrackers[i].nextIndex = rf.log.lastIndex() + 1
		// warning: cannot set the initial match index to the snapshot index since there might be new peers or way too lag-behind peers.
		rf.peerTrackers[i].matchIndex = 0
	}
}

func (rf *Raft) quorumActive() bool {
	activePeers := 1
	for i, tracker := range rf.peerTrackers {
		if i != rf.me && time.Since(tracker.lastAck) <= activeWindowWidth {
			activePeers++
		}
	}
	return 2*activePeers > len(rf.peers)
}
