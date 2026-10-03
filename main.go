package main

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

const (
	maxStateBytes = 64 << 20 // 64 MiB
	maxLockBytes  = 1 << 20
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type server struct {
	store Store
	auth  *authn
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "user" {
		userCmd(os.Args[2:])
		return
	}
	serve()
}

func serve() {
	listen := flag.String("listen", env("TFSTATE_LISTEN", ":8080"), "listen address")
	driver, dsn := dbFlags(flag.CommandLine)
	adminUser := flag.String("admin-user", env("TFSTATE_ADMIN_USER", ""), "optional bootstrap admin username (access to all projects)")
	adminPass := flag.String("admin-password", env("TFSTATE_ADMIN_PASSWORD", ""), "bootstrap admin password")
	keep := flag.Int("keep-versions", 0, "versions to retain per state (0 = keep all)")
	noAuth := flag.Bool("no-auth", false, "disable authentication (dev only)")
	certFile := flag.String("tls-cert", "", "TLS cert file (enables HTTPS)")
	keyFile := flag.String("tls-key", "", "TLS key file")
	flag.Parse()

	if (*adminUser == "") != (*adminPass == "") {
		log.Fatal("-admin-user and -admin-password must be set together")
	}

	store, err := OpenSQLStore(*driver, *dsn, *keep)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if !*noAuth && *adminUser == "" {
		if creds, err := store.ListCredentials(context.Background()); err == nil && len(creds) == 0 {
			log.Print("warning: no users configured, all requests will get 401. " +
				"Add one: tfstate-server user add -project <name> -user <name> -generate")
		}
	}

	h := (&server{store: store, auth: newAuthn(store, *noAuth, *adminUser, *adminPass)}).routes()

	srv := &http.Server{
		Addr:              *listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	log.Printf("listening on %s (db=%s)", *listen, *driver)
	if *certFile != "" {
		err = srv.ListenAndServeTLS(*certFile, *keyFile)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	const p = "/tfstate/{project}/{env}"
	mux.HandleFunc("GET "+p, s.get)
	mux.HandleFunc("GET "+p+"/versions", s.versions)
	mux.HandleFunc("POST "+p, s.post)
	mux.HandleFunc("DELETE "+p, s.del)
	mux.HandleFunc("LOCK "+p, s.lock)
	mux.HandleFunc("UNLOCK "+p, s.unlock)
	return logRequests(mux)
}

// ---- middleware ----

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d", r.Method, r.URL.Path, sw.code)
	})
}

// ---- handlers ----

// authz is the first call in every handler: authenticate (401), validate
// names (400), then check the caller may access this project (403).
func (s *server) authz(w http.ResponseWriter, r *http.Request) (project, env string, ok bool) {
	scope, ok := s.auth.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="tfstate", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", "", false
	}
	project, env = r.PathValue("project"), r.PathValue("env")
	if !nameRe.MatchString(project) || !nameRe.MatchString(env) {
		http.Error(w, "invalid project or env name", http.StatusBadRequest)
		return "", "", false
	}
	if scope != "*" && scope != project {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", "", false
	}
	return project, env, true
}

// get returns the latest state, or a specific one with ?version=N.
func (s *server) get(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.authz(w, r)
	if !ok {
		return
	}
	version := 0 // latest
	if v := r.URL.Query().Get("version"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "version must be a positive integer", http.StatusBadRequest)
			return
		}
		version = n
	}
	data, got, err := s.store.GetState(r.Context(), project, env, version)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound) // terraform treats 404 as empty state
		return
	}
	if err != nil {
		serverErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-State-Version", strconv.Itoa(got))
	w.Write(data)
}

// versions lists stored versions, newest first.
func (s *server) versions(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.authz(w, r)
	if !ok {
		return
	}
	vs, err := s.store.ListVersions(r.Context(), project, env)
	if err != nil {
		serverErr(w, err)
		return
	}
	if len(vs) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(vs)
}

func (s *server) post(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.authz(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStateBytes))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusRequestEntityTooLarge)
		return
	}
	if !json.Valid(body) {
		http.Error(w, "body is not valid JSON", http.StatusBadRequest)
		return
	}
	if want := r.Header.Get("Content-MD5"); want != "" {
		sum := md5.Sum(body)
		if base64.StdEncoding.EncodeToString(sum[:]) != want {
			http.Error(w, "Content-MD5 mismatch", http.StatusBadRequest)
			return
		}
	}
	// Terraform sends the held lock's ID as ?ID=<id> when writing locked state.
	v, err := s.store.PutState(r.Context(), project, env, body, r.URL.Query().Get("ID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("X-State-Version", strconv.Itoa(v))
	w.WriteHeader(http.StatusOK)
}

func (s *server) del(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.authz(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteState(r.Context(), project, env); err != nil {
		serverErr(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) lock(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.authz(w, r)
	if !ok {
		return
	}
	body, id, ok := lockBody(w, r)
	if !ok {
		return
	}
	if err := s.store.Lock(r.Context(), project, env, id, body); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) unlock(w http.ResponseWriter, r *http.Request) {
	project, env, ok := s.authz(w, r)
	if !ok {
		return
	}
	_, id, ok := lockBody(w, r)
	if !ok {
		return
	}
	if err := s.store.Unlock(r.Context(), project, env, id); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// lockBody reads the lock-info JSON Terraform sends and extracts its ID.
func lockBody(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLockBytes))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusRequestEntityTooLarge)
		return nil, "", false
	}
	var info struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(body, &info); err != nil || info.ID == "" {
		http.Error(w, "body must be lock info JSON with an ID", http.StatusBadRequest)
		return nil, "", false
	}
	return body, info.ID, true
}

// fail maps LockError to 423 (body = current lock info), everything else to 500.
func (s *server) fail(w http.ResponseWriter, err error) {
	var le *LockError
	if errors.As(err, &le) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		w.Write(le.Info)
		return
	}
	serverErr(w, err)
}

func serverErr(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
