// Package ondisk implements an encrypted on-disk storage backend with
// integrated revision control as well as automatic synchronization (soon).
package ondisk

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blang/semver"
	"github.com/gopasspw/gopass/internal/backend/crypto/age"
	"github.com/gopasspw/gopass/internal/backend/storage/ondisk/gjs"
	"github.com/gopasspw/gopass/internal/debug"
	"github.com/gopasspw/gopass/pkg/ctxutil"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	idxFile       = "index.gp1"
	idxBakFile    = "index.gp1.back"
	idxLockFile   = "index.gp1.lock"
	idxFileRemote = "index.gp1.remote"
	cfgRemote     = "remote.gp1"
	maxRev        = 256
	delTTL        = time.Hour * 24 * 365
)

// OnDisk is an on disk key-value store
type OnDisk struct {
	dir string
	idx *gjs.Store
	age *age.Age
	// TODO those should likely move to their own struct
	mux sync.Mutex
	mio *minio.Client
	mbu string // bucket
	mpf string // prefix
}

// New creates a new ondisk store
func New(ctx context.Context, baseDir string) (*OnDisk, error) {
	a, err := age.New()
	if err != nil {
		return nil, err
	}
	o := &OnDisk{
		dir: baseDir,
		age: a,
	}
	idx, err := o.loadOrCreate(ctx, baseDir)
	if err != nil {
		return nil, err
	}
	o.idx = idx
	if err := o.initRemote(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

// Path returns the on disk path
func (o *OnDisk) Path() string {
	return o.dir
}

func (o *OnDisk) initRemote(ctx context.Context) error {
	cfg, err := o.loadRemoteConfig(ctx)
	if err != nil {
		return err
	}
	if cfg.KeyID == "" {
		debug.Log("KeyID not set")
		return nil
	}
	if cfg.Secret == "" {
		debug.Log("Secret not set")
		return nil
	}
	if cfg.Host == "" {
		cfg.Host = "storage.googleapis.com"
	}
	if cfg.Bucket == "" {
		debug.Log("Bucket not set")
		return nil
	}
	o.mbu = cfg.Bucket
	o.mpf = cfg.Prefix

	mioClient, err := minio.New(cfg.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.KeyID, cfg.Secret, ""),
		Secure: cfg.SSL,
	})
	if err != nil {
		return err
	}
	o.mio = mioClient
	debug.Log("Remote initialized with host: %s - ssl: %t - bucket: %s - key id: %s - key: %s - prefix: %s", cfg.Host, cfg.SSL, o.mbu, cfg.KeyID, cfg.Secret, o.mpf)
	return nil
}

func (o *OnDisk) loadOrCreate(ctx context.Context, path string) (*gjs.Store, error) {
	path = filepath.Join(path, idxFile)
	buf, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &gjs.Store{
				Name:    filepath.Base(path),
				Entries: make(map[string]*gjs.Entry),
			}, nil
		}
		return nil, err
	}
	debug.Log("loading index from %s", path)
	return o.loadIndex(ctx, buf)
}

func (o *OnDisk) loadIndex(ctx context.Context, buf []byte) (*gjs.Store, error) {
	buf, err := o.age.Decrypt(ctx, buf)
	if err != nil {
		return nil, err
	}
	debug.Log("JSON: %s", string(buf))
	idx := &gjs.Store{}
	err = json.Unmarshal(buf, idx)
	return idx, err
}

func (o *OnDisk) saveIndex(ctx context.Context, recipients ...string) ([]byte, error) {
	buf, err := json.Marshal(o.idx)
	if err != nil {
		return nil, err
	}
	debug.Log("JSON: %s", string(buf))
	return o.age.Encrypt(ctx, buf, recipients)
}

func (o *OnDisk) saveIndexToDisk(ctx context.Context) error {
	buf, err := o.saveIndex(ctx)
	if err != nil {
		return err
	}
	fn := filepath.Join(o.dir, idxFile)
	os.Rename(fn, filepath.Join(o.dir, idxBakFile))
	debug.Log("saving index to %s (%d bytes)", fn, len(buf))
	return ioutil.WriteFile(fn, buf, 0600)
}

// Get returns an entry
func (o *OnDisk) Get(ctx context.Context, name string) ([]byte, error) {
	e, err := o.getEntry(name)
	if err != nil {
		return nil, err
	}
	r := e.Latest()
	if r == nil {
		return nil, fmt.Errorf("not found")
	}
	path := filepath.Join(o.dir, r.GetFilename())
	debug.Log("Reading %s from %s", name, path)
	return ioutil.ReadFile(path)
}

func filename(buf []byte) string {
	sum := fmt.Sprintf("%x", sha256.Sum256(buf))
	return filepath.Join(sum[0:2], sum[2:])
}

// Set creates a new revision for an entry
func (o *OnDisk) Set(ctx context.Context, name string, value []byte) error {
	fn := filename(value)
	fp := filepath.Join(o.dir, filename(value))
	if err := os.MkdirAll(filepath.Dir(fp), 0700); err != nil {
		return err
	}
	if err := ioutil.WriteFile(fp, value, 0600); err != nil {
		return err
	}
	debug.Log("Wrote %s to %s", name, fp)
	e := o.getOrCreateEntry(name)
	msg := "Updated " + fn
	if cm := ctxutil.GetCommitMessage(ctx); cm != "" {
		msg = cm
	}
	e.Revisions = append(e.Revisions, &gjs.Revision{
		Created:  gjs.New(ctxutil.GetCommitTimestamp(ctx)),
		Message:  msg,
		Filename: fn,
	})
	debug.Log("Added Revision for %s: %+v", name, e)
	o.idx.Entries[name] = e
	debug.Log("Local: %p %+v", o.idx, o.idx)
	if err := o.saveIndexToDisk(ctx); err != nil {
		return err
	}
	debug.Log("Local: %p %p %+v", o, o.idx, o.idx)
	return nil
}

// Exists checks if an entry exists
func (o *OnDisk) Exists(ctx context.Context, name string) bool {
	e, found := o.idx.Entries[name]
	if !found {
		return false
	}
	found = !e.IsDeleted()
	debug.Log("%s exists? %t in %+v", name, found, o.idx.Entries)
	return found
}

func (o *OnDisk) getEntry(name string) (*gjs.Entry, error) {
	em := o.idx.GetEntries()
	if em == nil {
		return nil, fmt.Errorf("%s not found (empty index)", name)
	}
	e, found := em[name]
	if !found {
		return nil, fmt.Errorf("%s not found", name)
	}
	return e, nil
}

func (o *OnDisk) getOrCreateEntry(name string) *gjs.Entry {
	if e, found := o.idx.Entries[name]; found && e != nil {
		return e
	}
	debug.Log("Created new Entry for %s", name)
	return &gjs.Entry{
		Name:      name,
		Revisions: make([]*gjs.Revision, 0, 1),
	}
}

// Delete removes an entry
func (o *OnDisk) Delete(ctx context.Context, name string) error {
	if !o.Exists(ctx, name) {
		debug.Log("Not adding tombstone for non-existing entry for %s", name)
		return nil
	}
	// add tombstone
	e := o.getOrCreateEntry(name)
	e.Delete(ctxutil.GetCommitMessage(ctx))
	o.idx.Entries[name] = e

	debug.Log("Added tombstone for %s", name)
	return o.saveIndexToDisk(ctx)
}

// List lists all entries
func (o *OnDisk) List(ctx context.Context, prefix string) ([]string, error) {
	res := make([]string, 0, len(o.idx.Entries))
	for k, v := range o.idx.Entries {
		if v.IsDeleted() {
			continue
		}
		if strings.HasPrefix(k, prefix) {
			res = append(res, k)
		}
	}
	sort.Strings(res)
	debug.Log("Entries: %+v", res)
	return res, nil
}

// IsDir is not supported
func (o *OnDisk) IsDir(ctx context.Context, name string) bool {
	return false
}

// Prune removes all entries with a given prefix
func (o *OnDisk) Prune(ctx context.Context, prefix string) error {
	l, _ := o.List(ctx, name)
	for _, e := range l {
		if err := o.Delete(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// Name returns ondisk
func (o *OnDisk) Name() string {
	return name
}

// Version returns 1.0.0
func (o *OnDisk) Version(context.Context) semver.Version {
	return semver.Version{Major: 1}
}

// String returns the name and path
func (o *OnDisk) String() string {
	return fmt.Sprintf("%s(path: %s)", name, o.dir)
}

// Compact will prune all deleted entries and truncate every other entry
// to the last 10 revisions.
func (o *OnDisk) Compact(ctx context.Context) error {
	for k, v := range o.idx.Entries {
		if v.IsDeleted() && time.Since(v.Latest().Time()) > delTTL {
			delete(o.idx.Entries, k)
			continue
		}
		sort.Sort(gjs.ByRevision(o.idx.Entries[k].Revisions))
		if len(o.idx.Entries[k].Revisions) > maxRev {
			o.idx.Entries[k].Revisions = o.idx.Entries[k].Revisions[0:maxRev]
		}
	}
	return o.saveIndexToDisk(ctx)
}
