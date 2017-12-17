// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/syzkaller/pkg/cover"
	"github.com/google/syzkaller/pkg/hash"
	"github.com/google/syzkaller/pkg/host"
	"github.com/google/syzkaller/pkg/ipc"
	. "github.com/google/syzkaller/pkg/log"
	"github.com/google/syzkaller/pkg/osutil"
	. "github.com/google/syzkaller/pkg/rpctype"
	"github.com/google/syzkaller/prog"
	"github.com/google/syzkaller/sys"
)

var (
	flagName    = flag.String("name", "", "unique name for manager")
	flagArch    = flag.String("arch", runtime.GOARCH, "target arch")
	flagManager = flag.String("manager", "", "manager rpc address")
	flagProcs   = flag.Int("procs", 1, "number of parallel test processes")
	flagLeak    = flag.Bool("leak", false, "detect memory leaks")
	flagOutput  = flag.String("output", "stdout", "write programs to none/stdout/dmesg/file")
	flagPprof   = flag.String("pprof", "", "address to serve pprof profiles")
	flagTest    = flag.Bool("test", false, "enable image testing mode") // used by syz-ci
)

var (
	manager *RpcClient
	target  *prog.Target

	signalMu     sync.RWMutex
	corpusSignal map[uint32]struct{}
	maxSignal    map[uint32]struct{}
	newSignal    map[uint32]struct{}

	corpusMu     sync.RWMutex
	corpus       []*prog.Prog
	corpusHashes map[hash.Sig]struct{}

	allTriaged            uint32
	noCover               bool
	faultInjectionEnabled bool
	compsSupported        bool
)

type Fuzzer struct {
	config      *ipc.Config
	execOpts    *ipc.ExecOpts
	procs       []*Proc
	gate        *ipc.Gate
	workQueue   *WorkQueue
	choiceTable *prog.ChoiceTable
	stats       [StatCount]uint64
}

type Stat int

const (
	StatGenerate Stat = iota
	StatFuzz
	StatCandidate
	StatTriage
	StatMinimize
	StatSmash
	StatHint
	StatSeed
	StatCount
)

func main() {
	debug.SetGCPercent(50)
	flag.Parse()
	switch *flagOutput {
	case "none", "stdout", "dmesg", "file":
	default:
		fmt.Fprintf(os.Stderr, "-output flag must be one of none/stdout/dmesg/file\n")
		os.Exit(1)
	}
	Logf(0, "fuzzer started")

	var err error
	target, err = prog.GetTarget(runtime.GOOS, *flagArch)
	if err != nil {
		Fatalf("%v", err)
	}

	shutdown := make(chan struct{})
	osutil.HandleInterrupts(shutdown)
	go func() {
		// Handles graceful preemption on GCE.
		<-shutdown
		Logf(0, "SYZ-FUZZER: PREEMPTED")
		os.Exit(1)
	}()

	if *flagTest {
		testImage(*flagManager, target)
		return
	}

	if *flagPprof != "" {
		go func() {
			err := http.ListenAndServe(*flagPprof, nil)
			Fatalf("failed to serve pprof profiles: %v", err)
		}()
	} else {
		runtime.MemProfileRate = 0
	}

	corpusSignal = make(map[uint32]struct{})
	maxSignal = make(map[uint32]struct{})
	newSignal = make(map[uint32]struct{})
	corpusHashes = make(map[hash.Sig]struct{})

	Logf(0, "dialing manager at %v", *flagManager)
	a := &ConnectArgs{*flagName}
	r := &ConnectRes{}
	if err := RpcCall(*flagManager, "Manager.Connect", a, r); err != nil {
		panic(err)
	}
	calls := buildCallList(target, r.EnabledCalls)
	ct := target.BuildChoiceTable(r.Prios, calls)
	for _, inp := range r.Inputs {
		addInput(inp)
	}
	for _, s := range r.MaxSignal {
		maxSignal[s] = struct{}{}
	}

	// This requires "fault-inject: support systematic fault injection" kernel commit.
	if fd, err := syscall.Open("/proc/self/fail-nth", syscall.O_RDWR, 0); err == nil {
		syscall.Close(fd)
		faultInjectionEnabled = true
	}

	config, execOpts, err := ipc.DefaultConfig()
	if err != nil {
		panic(err)
	}
	if calls[target.SyscallMap["syz_emit_ethernet"]] ||
		calls[target.SyscallMap["syz_extract_tcp_res"]] {
		config.Flags |= ipc.FlagEnableTun
	}
	if faultInjectionEnabled {
		config.Flags |= ipc.FlagEnableFault
	}
	noCover = config.Flags&ipc.FlagSignal == 0

	kcov := false
	kcov, compsSupported = checkCompsSupported()
	Logf(0, "kcov=%v, comps=%v", kcov, compsSupported)
	if r.NeedCheck {
		out, err := osutil.RunCmd(time.Minute, "", config.Executor, "version")
		if err != nil {
			panic(err)
		}
		vers := strings.Split(strings.TrimSpace(string(out)), " ")
		if len(vers) != 4 {
			panic(fmt.Sprintf("bad executor version: %q", string(out)))
		}
		a := &CheckArgs{
			Name:           *flagName,
			UserNamespaces: osutil.IsExist("/proc/self/ns/user"),
			FuzzerGitRev:   sys.GitRevision,
			FuzzerSyzRev:   target.Revision,
			ExecutorGitRev: vers[3],
			ExecutorSyzRev: vers[2],
			ExecutorArch:   vers[1],
		}
		a.Kcov = kcov
		if fd, err := syscall.Open("/sys/kernel/debug/kmemleak", syscall.O_RDWR, 0); err == nil {
			syscall.Close(fd)
			a.Leak = true
		}
		a.Fault = faultInjectionEnabled
		a.CompsSupported = compsSupported
		for c := range calls {
			a.Calls = append(a.Calls, c.Name)
		}
		if err := RpcCall(*flagManager, "Manager.Check", a, nil); err != nil {
			panic(err)
		}
	}

	// Manager.Connect reply can ve very large and that memory will be permanently cached in the connection.
	// So we do the call on a transient connection, free all memory and reconnect.
	// The rest of rpc requests have bounded size.
	debug.FreeOSMemory()
	if conn, err := NewRpcClient(*flagManager); err != nil {
		panic(err)
	} else {
		manager = conn
	}

	kmemleakInit()

	leakCallback := func() {
		if atomic.LoadUint32(&allTriaged) != 0 {
			// Scan for leaks once in a while (it is damn slow).
			kmemleakScan(true)
		}
	}
	if !*flagLeak {
		leakCallback = nil
	}
	needPoll := make(chan struct{}, 1)
	needPoll <- struct{}{}
	fuzzer := &Fuzzer{
		config:      config,
		execOpts:    execOpts,
		gate:        ipc.NewGate(2**flagProcs, leakCallback),
		workQueue:   newWorkQueue(*flagProcs, needPoll),
		choiceTable: ct,
	}

	for _, candidate := range r.Candidates {
		p, err := target.Deserialize(candidate.Prog)
		if err != nil {
			panic(err)
		}
		if noCover {
			corpusMu.Lock()
			corpus = append(corpus, p)
			corpusMu.Unlock()
		} else {
			fuzzer.workQueue.enqueue(&WorkCandidate{p, candidate.Minimized})
		}
	}

	for pid := 0; pid < *flagProcs; pid++ {
		proc, err := newProc(fuzzer, pid)
		if err != nil {
			Fatalf("failed to create proc: %v", err)
		}
		fuzzer.procs = append(fuzzer.procs, proc)
		go proc.loop()
	}

	var execTotal uint64
	var lastPoll time.Time
	var lastPrint time.Time
	ticker := time.NewTicker(3 * time.Second).C
	for {
		poll := false
		select {
		case <-ticker:
		case <-needPoll:
			poll = true
		}
		if *flagOutput != "stdout" && time.Since(lastPrint) > 10*time.Second {
			// Keep-alive for manager.
			Logf(0, "alive, executed %v", execTotal)
			lastPrint = time.Now()
		}
		if poll || time.Since(lastPoll) > 10*time.Second {
			needCandidates := fuzzer.workQueue.wantCandidates()
			if poll && !needCandidates {
				continue
			}

			a := &PollArgs{
				Name:           *flagName,
				NeedCandidates: needCandidates,
				Stats:          make(map[string]uint64),
			}
			signalMu.Lock()
			a.MaxSignal = make([]uint32, 0, len(newSignal))
			for s := range newSignal {
				a.MaxSignal = append(a.MaxSignal, s)
			}
			newSignal = make(map[uint32]struct{})
			signalMu.Unlock()
			for _, proc := range fuzzer.procs {
				a.Stats["exec total"] += atomic.SwapUint64(&proc.env.StatExecs, 0)
				a.Stats["executor restarts"] += atomic.SwapUint64(&proc.env.StatRestarts, 0)
			}
			stat := func(stat Stat, name string) {
				v := atomic.SwapUint64(&fuzzer.stats[stat], 0)
				a.Stats[name] = v
				execTotal += v
			}
			stat(StatGenerate, "exec gen")
			stat(StatFuzz, "exec fuzz")
			stat(StatCandidate, "exec candidate")
			stat(StatTriage, "exec triage")
			stat(StatMinimize, "exec minimize")
			stat(StatSmash, "exec smash")
			stat(StatHint, "exec hints")
			stat(StatSeed, "exec seeds")

			r := &PollRes{}
			if err := manager.Call("Manager.Poll", a, r); err != nil {
				panic(err)
			}
			Logf(1, "poll: candidates=%v inputs=%v signal=%v",
				len(r.Candidates), len(r.NewInputs), len(r.MaxSignal))
			if len(r.MaxSignal) != 0 {
				signalMu.Lock()
				for _, s := range r.MaxSignal {
					maxSignal[s] = struct{}{}
				}
				signalMu.Unlock()
			}
			for _, inp := range r.NewInputs {
				addInput(inp)
			}
			for _, candidate := range r.Candidates {
				p, err := target.Deserialize(candidate.Prog)
				if err != nil {
					panic(err)
				}
				if noCover {
					corpusMu.Lock()
					corpus = append(corpus, p)
					corpusMu.Unlock()
				} else {
					fuzzer.workQueue.enqueue(&WorkCandidate{p, candidate.Minimized})
				}
			}
			if len(r.Candidates) == 0 && atomic.LoadUint32(&allTriaged) == 0 {
				if *flagLeak {
					kmemleakScan(false)
				}
				atomic.StoreUint32(&allTriaged, 1)
			}
			if len(r.NewInputs) == 0 && len(r.Candidates) == 0 {
				lastPoll = time.Now()
			}
		}
	}
}

func buildCallList(target *prog.Target, enabledCalls string) map[*prog.Syscall]bool {
	calls := make(map[*prog.Syscall]bool)
	if enabledCalls != "" {
		for _, id := range strings.Split(enabledCalls, ",") {
			n, err := strconv.ParseUint(id, 10, 64)
			if err != nil || n >= uint64(len(target.Syscalls)) {
				panic(fmt.Sprintf("invalid syscall in -calls flag: %v", id))
			}
			calls[target.Syscalls[n]] = true
		}
	} else {
		for _, c := range target.Syscalls {
			calls[c] = true
		}
	}

	if supp, err := host.DetectSupportedSyscalls(target); err != nil {
		Logf(0, "failed to detect host supported syscalls: %v", err)
	} else {
		for c := range calls {
			if !supp[c] {
				Logf(1, "disabling unsupported syscall: %v", c.Name)
				delete(calls, c)
			}
		}
	}

	trans := target.TransitivelyEnabledCalls(calls)
	for c := range calls {
		if !trans[c] {
			Logf(1, "disabling transitively unsupported syscall: %v", c.Name)
			delete(calls, c)
		}
	}
	return calls
}

func addInput(inp RpcInput) {
	corpusMu.Lock()
	defer corpusMu.Unlock()
	signalMu.Lock()
	defer signalMu.Unlock()

	if noCover {
		panic("should not be called when coverage is disabled")
	}
	p, err := target.Deserialize(inp.Prog)
	if err != nil {
		panic(err)
	}
	sig := hash.Hash(inp.Prog)
	if _, ok := corpusHashes[sig]; !ok {
		corpus = append(corpus, p)
		corpusHashes[sig] = struct{}{}
	}
	if diff := cover.SignalDiff(maxSignal, inp.Signal); len(diff) != 0 {
		cover.SignalAdd(corpusSignal, diff)
		cover.SignalAdd(maxSignal, diff)
	}
}
