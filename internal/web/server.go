package web

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"sip-server/internal/sip"
	"sip-server/internal/storage"
	"time"
)

// Server は、Webサーバーの依存関係を保持します。
type Server struct {
	storage   *storage.Storage
	templates map[string]*template.Template
	realm     string
	sipServer *sip.SIPServer
}

// NewServer は、新しいWebサーバーインスタンスを作成します。
func NewServer(s *storage.Storage, realm string, sipServer *sip.SIPServer) (*Server, error) {
	templates, err := parseTemplates()
	if err != nil {
		return nil, fmt.Errorf("failed to parse templates: %w", err)
	}

	return &Server{
		storage:   s,
		templates: templates,
		realm:     realm,
		sipServer: sipServer,
	}, nil
}

// Run は、指定されたアドレスでWebサーバーを起動し、正常なシャットダウンを処理します。
func (s *Server) Run(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// コンテキストのキャンセルをリッスンするゴルーチンを起動します
	go func() {
		<-ctx.Done()
		// 正常にシャットダウンするために5秒のタイムアウトを与えます
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Webサーバーのシャットダウンエラー: %v", err)
		}
	}()

	// サーバーを起動します
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		// これはShutdownが呼び出されたときの期待されるエラーなので、nilを返すことができます。
		return nil
	}
	return err
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/users", s.handleUsers)
	mux.HandleFunc("/users/new", s.handleUsersNew)
	mux.HandleFunc("/sessions", s.handleSessions)
	mux.HandleFunc("/settings/guidance", s.handleGuidanceSettings)
}

func (s *Server) handleGuidanceSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGuidanceSettingsForm(w, r, nil)
	case http.MethodPost:
		s.handleGuidanceSettingsSubmit(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleGuidanceSettingsForm は、ガイダンス設定フォームを表示します。
// 成功またはエラーメッセージを渡すために追加のデータを受け入れます。
func (s *Server) handleGuidanceSettingsForm(w http.ResponseWriter, r *http.Request, data map[string]interface{}) {
	if data == nil {
		data = make(map[string]interface{})
	}

	// フォームに現在の値を事前入力します
	user, audio := s.sipServer.GetGuidanceSettings()
	data["GuidanceUser"] = user
	data["GuidanceAudio"] = audio

	tmpl := s.templates["guidance.html"]
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Error executing guidance template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// handleGuidanceSettingsSubmit は、ガイダンス設定の更新を処理します。
func (s *Server) handleGuidanceSettingsSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	guidanceUser := r.FormValue("guidance_user")
	guidanceAudio := r.FormValue("guidance_audio")

	if guidanceUser == "" || guidanceAudio == "" {
		data := map[string]interface{}{
			"Error": "SIPユーザーと音声ファイルパスの両方が必要です。",
		}
		s.handleGuidanceSettingsForm(w, r, data)
		return
	}

	// 設定をDBに保存します
	if err := s.storage.SetSetting("guidance_user", guidanceUser); err != nil {
		log.Printf("Error saving guidance_user setting: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if err := s.storage.SetSetting("guidance_audio", guidanceAudio); err != nil {
		log.Printf("Error saving guidance_audio setting: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// 実行中のSIPサーバーインスタンスを更新します
	s.sipServer.UpdateGuidanceSettings(guidanceUser, guidanceAudio)

	// 成功メッセージとともにフォームを再表示します
	data := map[string]interface{}{
		"Success": true,
	}
	s.handleGuidanceSettingsForm(w, r, data)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/users", http.StatusFound)
}

// parseTemplates は、templatesディレクトリ内のすべての.htmlファイルを解析し、
// テンプレート名を解析済みテンプレートにマッピングしたマップを返します。
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

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	sessions := s.sipServer.GetActiveSessions()
	tmpl := s.templates["sessions.html"]
	if err := tmpl.Execute(w, sessions); err != nil {
		log.Printf("Error executing template: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
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

	// RFC 2617によると、ダイジェスト認証のA1部分は username:realm:password です。
	// 保存されるパスワードは、この文字列のMD5ハッシュである必要があります。
	// H(A1) = MD5(username:realm:password)
	ha1 := fmt.Sprintf("%s:%s:%s", username, s.realm, password)
	ha1Hash := fmt.Sprintf("%x", md5.Sum([]byte(ha1)))

	user := &storage.User{
		Username: username,
		Password: ha1Hash, // HA1ハッシュを保存します
	}

	if err := s.storage.AddUser(user); err != nil {
		log.Printf("Error adding user: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/users", http.StatusFound)
}
