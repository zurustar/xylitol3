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
	storage   *storage.Storage
	templates map[string]*template.Template
	realm     string
}

// NewServer creates a new web server instance.
func NewServer(s *storage.Storage, realm string) (*Server, error) {
	templates, err := parseTemplates()
	if err != nil {
		return nil, fmt.Errorf("failed to parse templates: %w", err)
	}

	return &Server{
		storage:   s,
		templates: templates,
		realm:     realm,
	}, nil
}

// Run starts the web server on the given address.
func (s *Server) Run(addr string) error {
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

// parseTemplates parses all .html files in the templates directory and returns a map
// of template names to parsed templates.
func parseTemplates() (map[string]*template.Template, error) {
	templates := make(map[string]*template.Template)
	templateDir := filepath.Join("internal", "web", "templates")

	files, err := filepath.Glob(filepath.Join(templateDir, "*.html"))
	if err != nil {
		return nil, fmt.Errorf("failed to find template files: %w", err)
	}

	for _, file := range files {
		name := filepath.Base(file)
		tmpl, err := template.ParseFiles(file)
		if err != nil {
			return nil, fmt.Errorf("failed to parse template %s: %w", name, err)
		}
		templates[name] = tmpl
	}

	log.Printf("Successfully parsed %d templates", len(templates))
	return templates, nil
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

	tmpl := s.templates["users.html"]
	if err := tmpl.Execute(w, users); err != nil {
		log.Printf("Error executing template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *Server) handleUsersNewForm(w http.ResponseWriter, r *http.Request) {
	tmpl := s.templates["users_new.html"]
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

	if username == "" || password == "" {
		http.Error(w, "Username and password are required", http.StatusBadRequest)
		return
	}

	// As per RFC 2617, the A1 part of digest auth is username:realm:password
	// The password stored should be the MD5 hash of this string.
	// H(A1) = MD5(username:realm:password)
	ha1 := fmt.Sprintf("%s:%s:%s", username, s.realm, password)
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
