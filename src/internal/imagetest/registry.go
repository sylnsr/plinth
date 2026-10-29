package imagetest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Registry is the least of the OCI distribution API that skopeo needs to push, pull and inspect: blob uploads, blobs,
// manifests by tag and digest, and the tag list. Every request needs Basic auth with user:pass, so a test proves that
// skopeo uses the user's podman login.
type Registry struct {
	user, pass string
	mu         sync.Mutex
	blobs      map[string][]byte   // digest -> bytes
	uploads    map[string][]byte   // upload id -> bytes so far
	manifests  map[string]manifest // <name>@<digest> -> manifest
	tags       map[string]string   // <name>:<tag> -> digest
}

type manifest struct {
	mediaType string
	data      []byte
}

func NewRegistry(user, pass string) *Registry {
	return &Registry{user: user, pass: pass, blobs: map[string][]byte{}, uploads: map[string][]byte{},
		manifests: map[string]manifest{}, tags: map[string]string{}}
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// ManifestDigest is the digest a manifest of name has under tag, "" for none.
func (r *Registry) ManifestDigest(name, tag string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tags[name+":"+tag]
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if u, p, ok := req.BasicAuth(); !ok || u != r.user || p != r.pass {
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		http.Error(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`, http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/v2/")
	if path == "" {
		w.Write([]byte("{}"))
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	notFound := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"errors":[{"code":%q}]}`, code)
	}
	switch {
	case strings.HasSuffix(path, "/tags/list"):
		name := strings.TrimSuffix(path, "/tags/list")
		var tags []string
		for k := range r.tags {
			if n, t, _ := strings.Cut(k, ":"); n == name {
				tags = append(tags, t)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"name": name, "tags": tags})

	case strings.Contains(path, "/blobs/uploads/"):
		name, id, _ := strings.Cut(path, "/blobs/uploads/")
		body, _ := io.ReadAll(req.Body)
		switch req.Method {
		case http.MethodPost:
			b := make([]byte, 8)
			rand.Read(b)
			id = hex.EncodeToString(b)
			r.uploads[id] = body
			w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+id)
			w.Header().Set("Range", "0-0")
			w.Header().Set("Docker-Upload-UUID", id)
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPatch:
			data, ok := r.uploads[id]
			if !ok {
				notFound("BLOB_UPLOAD_UNKNOWN")
				return
			}
			r.uploads[id] = append(data, body...)
			w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+id)
			w.Header().Set("Range", fmt.Sprintf("0-%d", len(r.uploads[id])-1))
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPut:
			data, ok := r.uploads[id]
			if !ok {
				notFound("BLOB_UPLOAD_UNKNOWN")
				return
			}
			data = append(data, body...)
			delete(r.uploads, id)
			if d := req.URL.Query().Get("digest"); d != digestOf(data) {
				http.Error(w, `{"errors":[{"code":"DIGEST_INVALID"}]}`, http.StatusBadRequest)
				return
			}
			r.blobs[digestOf(data)] = data
			w.Header().Set("Location", "/v2/"+name+"/blobs/"+digestOf(data))
			w.Header().Set("Docker-Content-Digest", digestOf(data))
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}

	case strings.Contains(path, "/blobs/"):
		_, d, _ := strings.Cut(path, "/blobs/")
		data, ok := r.blobs[d]
		if !ok {
			notFound("BLOB_UNKNOWN")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Header().Set("Docker-Content-Digest", d)
		if req.Method == http.MethodGet {
			w.Write(data)
		}

	case strings.Contains(path, "/manifests/"):
		name, ref, _ := strings.Cut(path, "/manifests/")
		d := ref
		if !strings.HasPrefix(ref, "sha256:") {
			d = r.tags[name+":"+ref]
		}
		switch req.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(req.Body)
			d = digestOf(body)
			if strings.HasPrefix(ref, "sha256:") && ref != d {
				http.Error(w, `{"errors":[{"code":"DIGEST_INVALID"}]}`, http.StatusBadRequest)
				return
			}
			r.manifests[name+"@"+d] = manifest{mediaType: req.Header.Get("Content-Type"), data: body}
			if !strings.HasPrefix(ref, "sha256:") {
				r.tags[name+":"+ref] = d
			}
			w.Header().Set("Location", "/v2/"+name+"/manifests/"+d)
			w.Header().Set("Docker-Content-Digest", d)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet, http.MethodHead:
			m, ok := r.manifests[name+"@"+d]
			if !ok {
				notFound("MANIFEST_UNKNOWN")
				return
			}
			w.Header().Set("Content-Type", m.mediaType)
			w.Header().Set("Content-Length", fmt.Sprint(len(m.data)))
			w.Header().Set("Docker-Content-Digest", d)
			if req.Method == http.MethodGet {
				w.Write(m.data)
			}
		case http.MethodDelete:
			if _, ok := r.manifests[name+"@"+d]; !ok {
				notFound("MANIFEST_UNKNOWN")
				return
			}
			delete(r.manifests, name+"@"+d)
			for k, v := range r.tags {
				if v == d {
					delete(r.tags, k)
				}
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}

	default:
		notFound("NAME_UNKNOWN")
	}
}
