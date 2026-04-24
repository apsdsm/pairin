// Package control defines the TUI <-> supervisor protocol and implements
// both sides of it. Wire format is NDJSON over a unix-domain stream socket
// at .pairin/control.sock. Messages are typed by a "kind" field.
//
// Client -> supervisor messages are requests (restart a service, shut down,
// ask for a snapshot). Supervisor -> client messages are events (a service
// changed status, a new log line, a fresh snapshot). A client that connects
// always receives a snapshot as its first event; subsequent events follow.
package control

import (
	"encoding/json"
	"time"
)

// ----- Client -> Supervisor requests -----

// RequestKind identifies a client request.
type RequestKind string

const (
	ReqSnapshot RequestKind = "snapshot"
	ReqRestart  RequestKind = "restart"
	ReqStop     RequestKind = "stop"
	ReqStart    RequestKind = "start"
	ReqShutdown RequestKind = "shutdown"
)

// Request is the envelope for all client-to-supervisor messages.
type Request struct {
	Kind    RequestKind `json:"kind"`
	Service string      `json:"service,omitempty"`
}

// ----- Supervisor -> Client events -----

// EventKind identifies a supervisor event.
type EventKind string

const (
	EvtSnapshot EventKind = "snapshot"
	EvtStatus   EventKind = "status"
	EvtLog      EventKind = "log"
	EvtHealth   EventKind = "health"
	EvtShutdown EventKind = "shutdown"
)

// Event is the envelope for all supervisor-to-client messages.
// Only the fields relevant to Kind are populated.
type Event struct {
	Kind EventKind `json:"kind"`

	// Snapshot: full state sent on client connect.
	Snapshot *Snapshot `json:"snapshot,omitempty"`

	// Status: one service's lifecycle state changed.
	Status *StatusEvent `json:"status,omitempty"`

	// Log: one service emitted a new log line.
	Log *LogEvent `json:"log,omitempty"`

	// Health: one service's healthcheck flipped.
	Health *HealthEvent `json:"health,omitempty"`
}

// Snapshot is a point-in-time description of the supervised world.
type Snapshot struct {
	ConfigPath string            `json:"config_path"`
	ProjectName string           `json:"project_name"`
	StartedAt  time.Time         `json:"started_at"`
	Services   []ServiceSnapshot `json:"services"`
}

// ServiceSnapshot captures everything the TUI needs to render one service.
type ServiceSnapshot struct {
	Name         string `json:"name"`
	Short        string `json:"short"`
	Color        string `json:"color"`
	Dir          string `json:"dir"`
	Cmd          string `json:"cmd"`
	Status       string `json:"status"`
	PID          int    `json:"pid"`
	Branch       string `json:"branch"`
	Healthy      bool   `json:"healthy"`
	HasHealth    bool   `json:"has_health"`
	Adopted      bool   `json:"adopted"`
	LogFile      string `json:"log_file"`
	RestartCount int    `json:"restart_count"`
	MaxRestarts  int    `json:"max_restarts"`
	DependsOn    []string `json:"depends_on,omitempty"`
}

// StatusEvent is sent whenever a service transitions between states.
type StatusEvent struct {
	Service      string `json:"service"`
	Status       string `json:"status"`
	PID          int    `json:"pid"`
	Branch       string `json:"branch"`
	RestartCount int    `json:"restart_count"`
}

// LogEvent is sent for every new log line captured from a service.
type LogEvent struct {
	Service string `json:"service"`
	Line    string `json:"line"`
}

// HealthEvent is sent whenever a service's healthcheck result changes.
type HealthEvent struct {
	Service string `json:"service"`
	Healthy bool   `json:"healthy"`
}

// MarshalLine serializes v into a newline-terminated JSON frame.
func MarshalLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
