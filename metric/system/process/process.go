// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

//go:build (darwin && cgo) || freebsd || linux || windows || aix
// +build darwin,cgo freebsd linux windows aix

package process

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	psutil "github.com/shirou/gopsutil/process"

	"github.com/elastic/elastic-agent-libs/mapstr"
	"github.com/elastic/elastic-agent-libs/opt"
	"github.com/elastic/elastic-agent-libs/transform/typeconv"
	"github.com/elastic/elastic-agent-system-metrics/metric"
	"github.com/elastic/elastic-agent-system-metrics/metric/system/network"
	"github.com/elastic/elastic-agent-system-metrics/metric/system/resolve"
	"github.com/elastic/go-sysinfo"
	sysinfotypes "github.com/elastic/go-sysinfo/types"
)

import "C"

// ListStates is a wrapper that returns a list of processess with only the basic PID info filled out.
func ListStates(hostfs resolve.Resolver) ([]ProcState, error) {
	init := Stats{
		Hostfs:        hostfs,
		Procs:         []string{".*"},
		EnableCgroups: false,
		skipExtended:  true,
	}
	err := init.Init()
	if err != nil {
		return nil, fmt.Errorf("error initializing process collectors: %w", err)
	}

	// actually fetch the PIDs from the OS-specific code
	_, plist, err := init.FetchPids()
	if err != nil {
		return nil, fmt.Errorf("error gathering PIDs: %w", err)
	}

	return plist, nil
}

// GetPIDState returns the state of a given PID
// It will return ProcNotExist if the process was not found.
func GetPIDState(hostfs resolve.Resolver, pid int) (PidState, error) {
	// As psutils doesn't support AIX, we use GetInfoForPid directly.
	// It returns an ESRCH if no process is found.
	if runtime.GOOS == "aix" {
		state, err := GetInfoForPid(hostfs, pid)
		if err == syscall.ESRCH {
			// We assume that syscall.ESRCH is mapped for all GOOS
			// this package is compiled for. If not, we will have to give this
			// to a build-flag controlled function to check.
			return "", ProcNotExist
		}
		if err != nil {
			return "", fmt.Errorf("error getting PID info for %d: %w", pid, err)
		}

		return state.State, nil
	}

	// This library still doesn't have a good cross-platform way to distinguish between "does not eixst" and other process errors.
	// This is a fairly difficult problem to solve in a cross-platform way
	exists, err := psutil.PidExistsWithContext(context.Background(), int32(pid))
	if err != nil {
		return "", fmt.Errorf("Error trying to find process: %d: %w", pid, err)
	}
	if !exists {
		return "", ProcNotExist
	}

	//GetInfoForPid will return the smallest possible dataset for a PID
	procState, err := GetInfoForPid(hostfs, pid)
	if err != nil {
		return "", fmt.Errorf("error getting state info for pid %d: %w", pid, err)
	}

	return procState.State, nil
}

// Get fetches the configured processes and returns a list of formatted events and root ECS fields
func (procStats *Stats) Get() ([]mapstr.M, []mapstr.M, error) {
	//If the user hasn't configured any kind of process glob, return
	if len(procStats.Procs) == 0 {
		return nil, nil, nil
	}

	// actually fetch the PIDs from the OS-specific code
	pidMap, plist, err := procStats.FetchPids()

	if err != nil {
		return nil, nil, fmt.Errorf("error gathering PIDs: %w", err)
	}
	// We use this to track processes over time.
	procStats.ProcsMap.SetMap(pidMap)

	// filter the process list that will be passed down to users
	plist = procStats.includeTopProcesses(plist)

	// This is a holdover until we migrate this library to metricbeat/internal
	// At which point we'll use the memory code there.
	var totalPhyMem uint64
	if procStats.host != nil {
		memStats, err := procStats.host.Memory()
		if err != nil {
			procStats.logger.Warnf("Getting memory details: %v", err)
		} else {
			totalPhyMem = memStats.Total
		}

	}

	//Format the list to the MapStr type used by the outputs
	procs := []mapstr.M{}
	rootEvents := []mapstr.M{}

	for _, process := range plist {
		process := process
		// Add the RSS pct memory first
		process.Memory.Rss.Pct = GetProcMemPercentage(process, totalPhyMem)
		//Create the root event
		root := process.FormatForRoot()
		rootMap := mapstr.M{}
		_ = typeconv.Convert(&rootMap, root)

		proc, err := procStats.getProcessEvent(&process)
		if err != nil {
			return nil, nil, fmt.Errorf("error converting process for pid %d: %w", process.Pid.ValueOr(0), err)
		}

		procs = append(procs, proc)
		rootEvents = append(rootEvents, rootMap)
	}

	return procs, rootEvents, nil
}

// GetOne fetches process data for a given PID if its name matches the regexes provided from the host.
func (procStats *Stats) GetOne(pid int) (mapstr.M, error) {
	pidStat, _, err := procStats.pidFill(pid, false)
	if err != nil {
		return nil, fmt.Errorf("error fetching PID %d: %w", pid, err)
	}

	procStats.ProcsMap.SetPid(pid, pidStat)

	return procStats.getProcessEvent(&pidStat)
}

// GetSelf gets process info for the beat itself
func (procStats *Stats) GetSelf() (ProcState, error) {
	self := os.Getpid()

	pidStat, _, err := procStats.pidFill(self, false)
	if err != nil {
		return ProcState{}, fmt.Errorf("error fetching PID %d: %w", self, err)
	}

	procStats.ProcsMap.SetPid(self, pidStat)
	return pidStat, nil
}

// pidIter wraps a few lines of generic code that all OS-specific FetchPids() functions must call.
// this also handles the process of adding to the maps/lists in order to limit the code duplication in all the OS implementations
func (procStats *Stats) pidIter(pid int, procMap ProcsMap, proclist []ProcState) (ProcsMap, []ProcState) {
	status, saved, err := procStats.pidFill(pid, true)
	if err != nil {
		procStats.logger.Debugf("Error fetching PID info for %d, skipping: %s", pid, err)
		return procMap, proclist
	}
	if !saved {
		procStats.logger.Debugf("Process name does not match the provided regex; PID=%d; name=%s", pid, status.Name)
		return procMap, proclist
	}
	procMap[pid] = status
	proclist = append(proclist, status)

	return procMap, proclist
}

// pidFill is an entrypoint used by OS-specific code to fill out a pid.
// This in turn calls various OS-specific code to fill out the various bits of PID data
// This is done to minimize the code duplication between different OS implementations
// The second return value will only be false if an event has been filtered out
func (procStats *Stats) pidFill(pid int, filter bool) (ProcState, bool, error) {
	// Fetch proc state so we can get the name for filtering based on user's filter.

	// OS-specific entrypoint, get basic info so we can at least run matchProcess
	status, err := GetInfoForPid(procStats.Hostfs, pid)
	if err != nil {
		return status, true, fmt.Errorf("GetInfoForPid: %w", err)
	}
	if procStats.skipExtended {
		return status, true, nil
	}
	status = procStats.cacheCmdLine(status)

	// Filter based on user-supplied func
	if filter {
		if !procStats.matchProcess(status.Name) {
			return status, false, nil
		}
	}

	//If we've passed the filter, continue to fill out the rest of the metrics
	status, err = FillPidMetrics(procStats.Hostfs, pid, status, procStats.isWhitelistedEnvVar)
	if err != nil {
		return status, true, fmt.Errorf("FillPidMetrics: %w", err)
	}
	if len(status.Args) > 0 && status.Cmdline == "" {
		status.Cmdline = strings.Join(status.Args, " ")
	}

	//postprocess with cgroups and percentages
	last, ok := procStats.ProcsMap.GetPid(status.Pid.ValueOr(0))
	status.SampleTime = time.Now()
	if procStats.EnableCgroups {
		cgStats, err := procStats.cgroups.GetStatsForPid(status.Pid.ValueOr(0))
		if err != nil {
			return status, true, fmt.Errorf("cgroups.GetStatsForPid: %w", err)
		}
		status.Cgroup = cgStats
		if ok {
			status.Cgroup.FillPercentages(last.Cgroup, status.SampleTime, last.SampleTime)
		}
	} // end cgroups processor

	// network data
	if procStats.EnableNetwork {
		procHandle, err := sysinfo.Process(pid)
		// treat this as a soft error
		if err != nil {
			procStats.logger.Debugf("error initializing process handler for pid %d while trying to fetch network data: %w", pid, err)
		} else {
			procNet, ok := procHandle.(sysinfotypes.NetworkCounters)
			if ok {
				status.Network, err = procNet.NetworkCounters()
				if err != nil {
					procStats.logger.Debugf("error fetching network counters for process %d: %w", pid, err)
				}
			}
		}
	}

	if status.CPU.Total.Ticks.Exists() {
		status.CPU.Total.Value = opt.FloatWith(metric.Round(float64(status.CPU.Total.Ticks.ValueOr(0))))
	}
	if ok {
		status = GetProcCPUPercentage(last, status)
	}

	return status, true, nil
}

// cacheCmdLine fills out Env and arg metrics from any stored previous metrics for the pid
func (procStats *Stats) cacheCmdLine(in ProcState) ProcState {
	if previousProc, ok := procStats.ProcsMap.GetPid(in.Pid.ValueOr(0)); ok {
		if procStats.CacheCmdLine {
			in.Args = previousProc.Args
			in.Cmdline = previousProc.Cmdline
		}
		env := previousProc.Env
		in.Env = env
	}
	return in
}

// return a formatted MapStr of the process metrics
func (procStats *Stats) getProcessEvent(process *ProcState) (mapstr.M, error) {

	// Remove CPUTicks if needed
	if !procStats.CPUTicks {
		process.CPU.User.Ticks = opt.NewUintNone()
		process.CPU.System.Ticks = opt.NewUintNone()
		process.CPU.Total.Ticks = opt.NewUintNone()
	}

	proc := mapstr.M{}
	err := typeconv.Convert(&proc, process)

	if procStats.EnableNetwork && process.Network != nil {
		proc["network"] = network.MapProcNetCountersWithFilter(process.Network, procStats.NetworkMetrics)
	}

	return proc, err
}

// matchProcess checks if the provided process name matches any of the process regexes
func (procStats *Stats) matchProcess(name string) bool {
	for _, reg := range procStats.procRegexps {
		if reg.MatchString(name) {
			return true
		}
	}
	return false
}

// includeTopProcesses filters down the metrics based on top CPU or top Memory settings
func (procStats *Stats) includeTopProcesses(processes []ProcState) []ProcState {
	if !procStats.IncludeTop.Enabled ||
		(procStats.IncludeTop.ByCPU == 0 && procStats.IncludeTop.ByMemory == 0) {

		return processes
	}

	var result []ProcState
	if procStats.IncludeTop.ByCPU > 0 {
		numProcs := procStats.IncludeTop.ByCPU
		if len(processes) < procStats.IncludeTop.ByCPU {
			numProcs = len(processes)
		}

		sort.Slice(processes, func(i, j int) bool {
			return processes[i].CPU.Total.Pct.ValueOr(0) > processes[j].CPU.Total.Pct.ValueOr(0)
		})
		result = append(result, processes[:numProcs]...)
	}

	if procStats.IncludeTop.ByMemory > 0 {
		numProcs := procStats.IncludeTop.ByMemory
		if len(processes) < procStats.IncludeTop.ByMemory {
			numProcs = len(processes)
		}

		sort.Slice(processes, func(i, j int) bool {
			return processes[i].Memory.Rss.Bytes.ValueOr(0) > processes[j].Memory.Rss.Bytes.ValueOr(0)
		})
		for _, proc := range processes[:numProcs] {
			proc := proc
			if !isProcessInSlice(result, &proc) {
				result = append(result, proc)
			}
		}
	}

	return result
}

// isWhitelistedEnvVar returns true if the given variable name is a match for
// the whitelist. If the whitelist is empty it returns false.
func (procStats Stats) isWhitelistedEnvVar(varName string) bool {
	if len(procStats.envRegexps) == 0 {
		return false
	}

	for _, p := range procStats.envRegexps {
		if p.MatchString(varName) {
			return true
		}
	}
	return false
}
