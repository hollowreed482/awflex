// Package core is the awflex server: store, git cache, batch engine and REST API.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

const Version = "0.2.0"

// Run statuses. queued and running are live; the rest are final.
const (
	StQueued      = "queued"
	StRunning     = "running"
	StSuccessful  = "successful"
	StFailed      = "failed"
	StUnreachable = "unreachable"
	StError       = "error" // awflex/git/ansible could not run the play for this host
	StCanceled    = "canceled"
	StTimeout     = "timeout"
)

func isFinal(s string) bool { return s != StQueued && s != StRunning }

// Credential holds secrets injected into git and ansible. Never returned by the API.
// Every stored credential applies to every run; fields merge in name order.
type Credential struct {
	Name           string            `json:"name"`
	SSHUser        string            `json:"ssh_user,omitempty"`
	SSHPrivateKey  string            `json:"ssh_private_key,omitempty"`
	BecomePassword string            `json:"become_password,omitempty"`
	VaultPassword  string            `json:"vault_password,omitempty"`
	GitUser        string            `json:"git_user,omitempty"`
	GitToken       string            `json:"git_token,omitempty"`
	GitSSHKey      string            `json:"git_ssh_key,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Managed        string            `json:"managed,omitempty"` // "file" when loaded from the mounted dir
}

// Request is what to run. All hosts of one ansible run share one Request.
type Request struct {
	Ref       string         `json:"ref,omitempty"`
	Playbook  string         `json:"playbook,omitempty"`
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
	Tags      []string       `json:"tags,omitempty"`
	SkipTags  []string       `json:"skip_tags,omitempty"`
	Check     bool           `json:"check,omitempty"`
	Source    string         `json:"source,omitempty"` // hook | api
	From      string         `json:"from,omitempty"`   // caller address
	Group     string         `json:"group,omitempty"`  // hosts with the same group run in one ansible process
}

// groupKey makes requests with identical parameters share a batch.
func (r Request) groupKey() string {
	b, _ := json.Marshal([]any{r.Ref, r.Playbook, r.ExtraVars, r.Tags, r.SkipTags, r.Check})
	s := sha256.Sum256(b)
	return "g-" + hex.EncodeToString(s[:8])
}

type Stats struct {
	OK          int `json:"ok"`
	Changed     int `json:"changed"`
	Failed      int `json:"failed"`
	Unreachable int `json:"unreachable"`
	Skipped     int `json:"skipped"`
	Rescued     int `json:"rescued"`
	Ignored     int `json:"ignored"`
}

type FailedTask struct {
	Task string `json:"task"`
	Msg  string `json:"msg,omitempty"`
}

// Run is the last (or current) run on one host. Only one is kept per host.
//
// The record is also the durable queue and the finish marker: it is written
// as queued when accepted, running when ansible starts, and a final status
// when ansible reports the host. A record still queued or running at startup
// never finished, so it is run again.
type Run struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"` // times ansible was started for this request
	Request
	Commit      string       `json:"commit,omitempty"`
	BatchID     string       `json:"batch_id,omitempty"`
	QueuedAt    time.Time    `json:"queued_at"`
	StartedAt   *time.Time   `json:"started_at,omitempty"`
	FinishedAt  *time.Time   `json:"finished_at,omitempty"`
	Stats       *Stats       `json:"stats,omitempty"`
	FailedTasks []FailedTask `json:"failed_tasks,omitempty"`
	Error       string       `json:"error,omitempty"`
	Next        *Request     `json:"next,omitempty"` // requested while this run was in flight; starts when it ends
}
