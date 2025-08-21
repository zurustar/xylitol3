package web

import (
	"crypto/md5"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"sip-server/internal/storage"
)

// Server holds the dependencies for the web server.
type Server struct {
	storage *storage.Storage
	// templates map[string]*template.Template
	// For simplicity, we'll parse templates on each request.
	// In a production app, you would parse them once at startup.
}

// NewServer creates a new web server instance.
func NewServer(s *storage.Storage) *Server {
	return &Server{storage: s}
}

// Run starts the web server on the given address.
func (s *Server) Run(addr string) error {
	log.Printf("Starting web server on %s", addr)
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	return http.ListenAndServe(addr, mux)
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/users", s.handleUsers)
	mux.HandleFunc("/users/new", s.handleUsersNew)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/users", http.StatusFound)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleUsersList(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleUsersNew(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleUsersNewForm(w, r)
	case http.MethodPost:
		s.handleUsersNewSubmit(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleUsersList(w http.ResponseWriter, r *http.Request) {
	users, err := s.storage.GetAllUsers()
	if err != nil {
		log.Printf("Error getting users: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// For simplicity, defining template path relative to this file.
	// A more robust solution might use an embedded filesystem or a configurable path.
	templatePath := filepath.Join("internal", "web", "templates", "users.html")
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		log.Printf("Error parsing template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := tmpl.Execute(w, users); err != nil {
		log.Printf("Error executing template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *Server) handleUsersNewForm(w http.ResponseWriter, r *http.Request) {
	templatePath := filepath.Join("internal", "web", "templates", "users_new.html")
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		log.Printf("Error parsing template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, nil); err != nil {
		log.Printf("Error executing template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *Server) handleUsersNewSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	realm := r.FormValue("realm") // Usually a domain like 'example.com'

	if username == "" || password == "" || realm == "" {
		http.Error(w, "Username, password, and realm are required", http.StatusBadRequest)
		return
	}

	// As per RFC 2617, the A1 part of digest auth is username:realm:password
	// The password stored should be the MD5 hash of this string.
	// H(A1) = MD5(username:realm:password)
	ha1 := fmt.Sprintf("%s:%s:%s", username, realm, password)
	ha1Hash := fmt.Sprintf("%x", md5.Sum([]byte(ha1)))

	user := &storage.User{
		Username: username,
		Password: ha1Hash, // Storing the HA1 hash
	}

	if err := s.storage.AddUser(user); err != nil {
		log.Printf("Error adding user: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/users", http.StatusFound)
}
