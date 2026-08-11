package harness

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"
)

type MemStore = memStore

type BackendBuckets = backendBuckets

func NewMemStore(endpoint, accessKeyID, secretAccessKey string, keys []*store.Key) *MemStore {
	return newMemStore(backendConfig{
		endpoint:        endpoint,
		accessKeyID:     accessKeyID,
		secretAccessKey: secretAccessKey,
	}, keys)
}

func (m *memStore) Claim(name, tenant string) (string, bool) { return m.claim(name, tenant) }
func (m *memStore) Owner(name string) (string, bool)         { return m.owner(name) }
func (m *memStore) Remove(name string)                       { m.remove(name) }

func NewInterceptor(st *MemStore, backend BackendBuckets, next http.Handler) http.Handler {
	return &bucketInterceptor{
		verifier: sigv4.NewVerifier(),
		store:    st,
		backend:  backend,
		next:     next,
		logger:   &jsonLogger{enc: json.NewEncoder(io.Discard)},
	}
}

var DefaultKeys = defaultKeys
