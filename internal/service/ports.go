package service

import (
	"sandboxd/internal/model"
	"sandboxd/internal/ports"
)

// PortsEnabled reports whether native port discovery is configured on. It is ON
// by default (see config.PortsConfig): read-only introspection of the tenant's
// OWN workspace, which the tenant otherwise cannot see from inside, because the
// curated /proc carries no /proc/net and neither ss nor netstat is installed.
func (s *Service) PortsEnabled() bool { return s.cfg.Ports.Enabled }

// PortsResult is a workspace's discovered services: the listening ports, the
// unix sockets (a separate section), and the honesty flags for the bounded read.
type PortsResult struct {
	Ports     []ports.Endpoint
	Count     int
	Unix      []ports.UnixSocket
	UnixCount int
	// Complete is false when the bounded read hit a cap, so the list is a
	// partial view rather than a total one.
	Complete bool
	Notes    []string
	// Bounded work actually performed, reported so a client (and a gate) can see
	// the cost was bounded.
	PidsScanned int
	FDsScanned  int
}

// ListPorts returns the workspace's listening endpoints and their owning
// processes. It builds ON the process registry (the same one GET
// /v1/workspaces/{id}/processes serves), so the two can never disagree about
// which processes exist: the candidate pids are exactly the workspace's live
// processes.
//
// Attribution: /proc/<pid>/net/* gives a socket inode, and the owner is the
// process whose /proc/<pid>/fd contains socket:[<inode>]. The scan is bounded
// (config.PortsConfig.Limits) and only ever runs over the workspace's own pids,
// so it never leaks the host's or another tenant's services. An endpoint whose
// owner cannot be resolved is reported unattributed, never dropped and never
// guessed.
func (s *Service) ListPorts(w *model.Workspace) (PortsResult, error) {
	if !s.cfg.Ports.Enabled {
		return PortsResult{}, model.New(404, model.CodePortsDisabled, "port discovery is disabled")
	}
	procs, err := s.ListProcesses(w)
	if err != nil {
		return PortsResult{}, err
	}
	// Candidate pids and their metadata, in registry order. A pid that vanishes
	// before the read is simply not read.
	refs := make(map[int]ports.ProcessRef, len(procs))
	pids := make([]int, 0, len(procs))
	for _, p := range procs {
		refs[p.PID] = ports.ProcessRef{PID: p.PID, Kind: p.Kind, Argv: p.Argv}
		pids = append(pids, p.PID)
	}
	maxPids, maxFDs := s.cfg.Ports.Limits()
	tables := s.runner.NetTablesForPids(pids, ports.Limits{MaxPids: maxPids, MaxFDsPerPID: maxFDs})
	res := ports.Build(tables, refs)
	out := PortsResult{
		Ports:       res.Ports,
		Count:       res.Count,
		Unix:        res.Unix,
		UnixCount:   res.UnixCount,
		Complete:    res.Complete,
		Notes:       res.Notes,
		PidsScanned: res.PidsScanned,
		FDsScanned:  res.FDsScanned,
	}
	// Unix sockets are a separate section only when configured; the flag never
	// mixes them into the port list.
	if !s.cfg.Ports.IncludeUnix {
		out.Unix = nil
		out.UnixCount = 0
	}
	return out, nil
}

// PortsStatus reports the capability truthfully for /v1/meta: whether the
// route is served and the bounds in force.
func (s *Service) PortsStatus() (enabled bool, maxPids, maxFDs int, includeUnix bool) {
	maxPids, maxFDs = s.cfg.Ports.Limits()
	return s.cfg.Ports.Enabled, maxPids, maxFDs, s.cfg.Ports.IncludeUnix
}

// PortReads returns how many port-table reads the process has performed. It is a
// monotonic diagnostic used by a gate to prove the read is on-demand only (it
// does not move across a metrics tick) while staying non-vacuous (it moves on a
// /ports request).
func (s *Service) PortReads() int64 { return s.runner.PortReads() }
