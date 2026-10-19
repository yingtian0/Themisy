package policy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"
)

// CandidateCatalog is an operator-local artifact cache, not a deployment or
// authorization store. Its directory must be exclusively operator-controlled.
// There is deliberately no active pointer, version alias, target or apply API.
type CandidateCatalog struct {
	root   *os.Root
	issuer string
	keys   BundleKeyResolver
	now    func() time.Time
}

const maxCandidateBytes = 2 << 20

var candidateHash = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// OpenCandidateCatalog opens an existing directory. Trust and observation time
// come from operator configuration, never from the candidate or its filename.
func OpenCandidateCatalog(directory, issuer string, keys BundleKeyResolver) (*CandidateCatalog, error) {
	if issuer == "" || keys == nil {
		return nil, ErrInvalidBundle
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	return &CandidateCatalog{root: root, issuer: issuer, keys: keys, now: time.Now}, nil
}

func (c *CandidateCatalog) Close() error { return c.root.Close() }

// Register verifies and compiles without network capabilities, then atomically
// publishes an immutable hash-addressed object. A repeated registration validates
// the existing object instead of overwriting or repairing it.
func (c *CandidateCatalog) Register(ctx context.Context, bundle Bundle) (string, error) {
	data, err := json.Marshal(normalizedBundle(bundle))
	if err != nil {
		return "", err
	}
	if len(data) > maxCandidateBytes {
		return "", fmt.Errorf("POLICY_CANDIDATE_TOO_LARGE")
	}
	// Decode a private copy before validation; retain no caller-owned slices.
	verified, err := c.decode(ctx, data, bundle.Hash)
	if err != nil {
		return "", err
	}
	name := verified.Hash[len("sha256:"):] + ".json"
	temporary := ".candidate-" + rand.Text()
	file, err := c.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer c.root.Remove(temporary)
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Link is create-if-absent: concurrent processes cannot replace an object.
	if err := c.root.Link(temporary, name); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if _, err := c.Get(ctx, verified.Hash); err != nil {
			return "", err
		}
	}
	// A failed directory sync is an error even if publication was visible. A
	// retry is safe because it revalidates the same immutable object.
	dir, err := c.root.Open(".")
	if err != nil {
		return "", err
	}
	err = errors.Join(dir.Sync(), dir.Close())
	if err != nil {
		return "", err
	}
	return verified.Hash, nil
}

// Get revalidates trust, hash, expiry and offline compilation on every read.
// A previous successful registration is not continuing approval or validity.
func (c *CandidateCatalog) Get(ctx context.Context, hash string) (Bundle, error) {
	if !candidateHash.MatchString(hash) {
		return Bundle{}, ErrInvalidBundle
	}
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	name := hash[len("sha256:"):] + ".json"
	info, err := c.root.Lstat(name)
	if err != nil {
		return Bundle{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxCandidateBytes {
		return Bundle{}, ErrInvalidBundle
	}
	file, err := c.root.Open(name)
	if err != nil {
		return Bundle{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxCandidateBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return Bundle{}, err
	}
	return c.decode(ctx, data, hash)
}

func (c *CandidateCatalog) decode(ctx context.Context, data []byte, hash string) (Bundle, error) {
	if !candidateHash.MatchString(hash) || len(data) > maxCandidateBytes {
		return Bundle{}, ErrInvalidBundle
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var bundle Bundle
	if err := decoder.Decode(&bundle); err != nil {
		return Bundle{}, ErrInvalidBundle
	}
	if err := decoder.Decode(new(any)); err != io.EOF || bundle.Hash != hash {
		return Bundle{}, ErrInvalidBundle
	}
	if _, err := newOfflineShadowOPA(ctx, bundle, c.issuer, c.keys, c.now().UTC()); err != nil {
		return Bundle{}, err
	}
	// Compilation may span the validity boundary. Recheck before returning.
	if err := VerifyBundle(ctx, bundle, c.issuer, c.keys, c.now().UTC()); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}
