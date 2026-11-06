package core

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

func newID() string {
	b := make([]byte, 5)
	rand.Read(b)
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b)
}

// Store keeps credentials and the last run per host. Everything is a small
// file under the data dir; there is no database.
type Store struct {
	dir  string
	aead cipher.AEAD

	mu       sync.RWMutex
	creds    map[string]*Credential
	extCreds map[string]*Credential
	runs     map[string]*Run
}

func OpenStore(dir, secretKey string) (*Store, error) {
	s := &Store{dir: dir, creds: map[string]*Credential{}, extCreds: map[string]*Credential{}, runs: map[string]*Run{}}
	if secretKey != "" {
		k := sha256.Sum256([]byte(secretKey))
		blk, err := aes.NewCipher(k[:])
		if err != nil {
			return nil, err
		}
		if s.aead, err = cipher.NewGCM(blk); err != nil {
			return nil, err
		}
	}
	for _, d := range []string{"credentials", "runs", "batches"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	return s, s.load()
}

// writeAtomic replaces path so a crash leaves either the old or the new file.
// The fsync is what makes the on-disk queue trustworthy after a node failure.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := fmt.Sprintf("%s.%s.tmp", path, newID())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func jsonFiles(dir string) map[string][]byte {
	out := map[string][]byte{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".json") {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
			out[strings.TrimSuffix(n, ".json")] = b
		}
	}
	return out
}

func (s *Store) load() error {
	for name, b := range jsonFiles(filepath.Join(s.dir, "credentials")) {
		c, err := s.unseal(b)
		if err != nil {
			return fmt.Errorf("credential %s: %w", name, err)
		}
		c.Name = name
		s.creds[name] = c
	}
	for host, b := range jsonFiles(filepath.Join(s.dir, "runs")) {
		var r Run
		if json.Unmarshal(b, &r) == nil && r.Host == host {
			s.runs[host] = &r
		}
	}
	// Leftovers from a write interrupted by a crash.
	for _, d := range []string{"credentials", "runs"} {
		if m, _ := filepath.Glob(filepath.Join(s.dir, d, "*.tmp")); m != nil {
			for _, f := range m {
				os.Remove(f)
			}
		}
	}
	return nil
}

// LoadExternal (re)reads the read-only mounted credentials dir. Kubelet swaps
// a mounted Secret's contents in place, so this is polled.
func (s *Store) LoadExternal(dir string) []error {
	if dir == "" {
		return nil
	}
	var errs []error
	ec := map[string]*Credential{}
	for name, b := range jsonFiles(dir) {
		var c Credential
		if err := json.Unmarshal(b, &c); err != nil || !nameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("credential file %s.json: invalid", name))
			continue
		}
		c.Name, c.Managed = name, "file"
		ec[name] = &c
	}
	s.mu.Lock()
	s.extCreds = ec
	s.mu.Unlock()
	return errs
}

// ---- credentials

type sealed struct {
	Sealed string `json:"sealed"`
}

func (s *Store) seal(c *Credential) ([]byte, error) {
	plain, _ := json.Marshal(c)
	if s.aead == nil {
		return plain, nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return json.Marshal(sealed{base64.StdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, plain, nil))})
}

func (s *Store) unseal(b []byte) (*Credential, error) {
	var sl sealed
	if json.Unmarshal(b, &sl) == nil && sl.Sealed != "" {
		if s.aead == nil {
			return nil, errors.New("sealed, but AWFLEX_SECRET_KEY is not set")
		}
		raw, err := base64.StdEncoding.DecodeString(sl.Sealed)
		if err != nil || len(raw) < s.aead.NonceSize() {
			return nil, errors.New("corrupt sealed credential")
		}
		n := s.aead.NonceSize()
		plain, err := s.aead.Open(nil, raw[:n], raw[n:], nil)
		if err != nil {
			return nil, errors.New("cannot unseal: wrong AWFLEX_SECRET_KEY?")
		}
		b = plain
	}
	var c Credential
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ListCredentials returns every credential in name order; mounted files win
// over API-managed ones of the same name.
func (s *Store) ListCredentials() []*Credential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := map[string]*Credential{}
	for n, c := range s.creds {
		m[n] = c
	}
	for n, c := range s.extCreds {
		m[n] = c
	}
	out := make([]*Credential, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var errManaged = errors.New("managed by a mounted file; change the Kubernetes Secret instead")

func (s *Store) PutCredential(c *Credential) error {
	c.Managed = ""
	if !nameRe.MatchString(c.Name) {
		return errors.New("name must match " + nameRe.String())
	}
	for k := range c.Env {
		if !envNameRe.MatchString(k) || strings.HasPrefix(k, "AWFLEX_") {
			return fmt.Errorf("invalid env name %q", k)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.extCreds[c.Name] != nil {
		return errManaged
	}
	b, err := s.seal(c)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.dir, "credentials", c.Name+".json"), b, 0o600); err != nil {
		return err
	}
	s.creds[c.Name] = c
	return nil
}

func (s *Store) DeleteCredential(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.extCreds[name] != nil {
		return errManaged
	}
	if s.creds[name] == nil {
		return os.ErrNotExist
	}
	delete(s.creds, name)
	return os.Remove(filepath.Join(s.dir, "credentials", name+".json"))
}

// ---- runs: exactly one record and one log per host

func (s *Store) runPath(host, ext string) string { return filepath.Join(s.dir, "runs", host+ext) }

func (s *Store) GetRun(host string) *Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runs[host]
}

// SaveRun replaces the host's record. Stored runs are immutable; callers pass a fresh copy.
func (s *Store) SaveRun(r *Run) error {
	b, _ := json.Marshal(r)
	err := writeAtomic(s.runPath(r.Host, ".json"), b, 0o600)
	s.mu.Lock()
	s.runs[r.Host] = r
	s.mu.Unlock()
	return err
}

// Mutate copies the stored run, applies fn and saves it. The engine serializes callers.
func (s *Store) Mutate(host string, fn func(*Run)) *Run {
	cur := s.GetRun(host)
	if cur == nil {
		return nil
	}
	cp := *cur
	fn(&cp)
	s.SaveRun(&cp)
	return &cp
}

func (s *Store) DeleteRun(host string) {
	s.mu.Lock()
	delete(s.runs, host)
	s.mu.Unlock()
	os.Remove(s.runPath(host, ".json"))
	os.Remove(s.runPath(host, ".log"))
}

func (s *Store) ListRuns(status string) []*Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*Run{}
	for _, r := range s.runs {
		if status == "" || r.Status == status {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

func (s *Store) WriteLog(host string, data []byte) { writeAtomic(s.runPath(host, ".log"), data, 0o600) }

func (s *Store) ReadLog(host string) ([]byte, error) { return os.ReadFile(s.runPath(host, ".log")) }

func (s *Store) batchLogPath(id string) string { return filepath.Join(s.dir, "batches", id+".log") }

// gcBatchLogs drops raw ansible output that is no longer any host's last run.
func (s *Store) gcBatchLogs(keep map[string]bool) {
	s.mu.RLock()
	for _, r := range s.runs {
		if r.BatchID != "" {
			keep[r.BatchID] = true
		}
	}
	s.mu.RUnlock()
	ents, _ := os.ReadDir(filepath.Join(s.dir, "batches"))
	for _, e := range ents {
		if id := strings.TrimSuffix(e.Name(), ".log"); !keep[id] {
			os.Remove(filepath.Join(s.dir, "batches", e.Name()))
		}
	}
}
