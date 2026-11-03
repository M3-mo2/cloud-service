package api

import (
	"net/http"

	"sandboxd/internal/ports"
)

// portEndpointJSON is one listening endpoint in the port/service inventory.
type portEndpointJSON struct {
	Port       int      `json:"port"`
	Protocol   string   `json:"protocol"`
	Address    string   `json:"address"`
	Scope      string   `json:"scope"`
	State      string   `json:"state"`
	Attributed bool     `json:"attributed"`
	PID        int      `json:"pid,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Label      string   `json:"label,omitempty"`
	Argv       []string `json:"argv,omitempty"`
	Owners     []int    `json:"owners,omitempty"`
	Inode      uint64   `json:"inode,omitempty"`
	IPv6       bool     `json:"ipv6,omitempty"`
}

// unixSocketJSON is one unix-domain socket. It is reported in a SEPARATE section
// and never mixed into the port list, because a unix socket is not a reachable
// port.
type unixSocketJSON struct {
	Path       string `json:"path,omitempty"`
	Type       string `json:"type"`
	State      string `json:"state"`
	Attributed bool   `json:"attributed"`
	PID        int    `json:"pid,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Label      string `json:"label,omitempty"`
	Inode      uint64 `json:"inode,omitempty"`
}

// handleListPorts returns the workspace's listening ports and the process
// serving each one. It is an ON-DEMAND read of live state (never cached, never
// part of the metrics stream). An empty list is an honest
// {"ports":[],"count":0}, never an error; an endpoint whose owner cannot be
// resolved is reported with attributed=false rather than dropped.
//
// IMPORTANT for clients: a port in this list is listening INSIDE the workspace's
// private network namespace. It does NOT mean the port is reachable from the
// outside. Publishing a port to the public internet is a separate feature
// (quick tunnels, gated on limits.allow_tunnels AND tunnels.enabled), so a
// client must never tell a user their app is public merely because it appears
// here.
func (s *Server) handleListPorts(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.ownedWorkspace(w, r)
	if !ok {
		return
	}
	res, err := s.svc.ListPorts(ws)
	if err != nil {
		writeErrorWithCode(w, err)
		return
	}
	out := make([]portEndpointJSON, 0, len(res.Ports))
	for _, e := range res.Ports {
		out = append(out, toPortJSON(e))
	}
	unix := make([]unixSocketJSON, 0, len(res.Unix))
	for _, u := range res.Unix {
		unix = append(unix, toUnixJSON(u))
	}
	notes := res.Notes
	if notes == nil {
		notes = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace_id": ws.ID,
		"ports":        out,
		"count":        res.Count,
		"unix":         unix,
		"unix_count":   res.UnixCount,
		// complete is false when the bounded scan hit a cap, so the list is a
		// partial view and not a total one (the same honesty rule the disk
		// figure uses).
		"complete":     res.Complete,
		"notes":        notes,
		"pids_scanned": res.PidsScanned,
		"fds_scanned":  res.FDsScanned,
		// A plain, non-negotiable note so a client cannot mistake this for a
		// reachability answer.
		"reachability": "ports are listening inside the workspace's private network namespace; this does not mean they are reachable from outside. Publishing a port requires a tunnel (limits.allow_tunnels and tunnels.enabled).",
	})
}

func toPortJSON(e ports.Endpoint) portEndpointJSON {
	return portEndpointJSON{
		Port:       e.Port,
		Protocol:   e.Protocol,
		Address:    e.Address,
		Scope:      e.Scope,
		State:      e.State,
		Attributed: e.Attributed,
		PID:        e.PID,
		Kind:       e.Kind,
		Label:      e.Label,
		Argv:       e.Argv,
		Owners:     e.Owners,
		Inode:      e.Inode,
		IPv6:       e.Proto6,
	}
}

func toUnixJSON(u ports.UnixSocket) unixSocketJSON {
	return unixSocketJSON{
		Path:       u.Path,
		Type:       u.Type,
		State:      u.State,
		Attributed: u.Attributed,
		PID:        u.PID,
		Kind:       u.Kind,
		Label:      u.Label,
		Inode:      u.Inode,
	}
}
