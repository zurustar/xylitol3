package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sip-server/internal/sip"
	"sip-server/internal/storage"
	"sip-server/internal/web"
	"strings"
	"syscall"

	"golang.org/x/sync/errgroup"
)

func main() {
	// --- 設定 ---
	var (
		webAddr = flag.String("web.addr", ":8080", "Web UIサーバーのアドレス")
		sipAddr = flag.String("sip.addr", ":5060", "SIPサーバーのアドレス")
		dbPath  = flag.String("db.path", "sip_users.db", "SQLiteデータベースファイルへのパス")
		realm   = flag.String("sip.realm", "go-sip-server", "認証用のSIPレルム")
	)
	flag.Parse()

	// --- アプリケーションのセットアップ ---
	log.Println("アプリケーションを初期化しています...")

	// ストレージを初期化
	s, err := storage.NewStorage(*dbPath)
	if err != nil {
		log.Fatalf("ストレージの初期化に失敗しました: %v", err)
	}
	defer s.Close()
	log.Printf("ストレージをデータベースファイルで初期化しました: %s", *dbPath)

	// ガイダンス設定をロードまたは初期化
	guidanceUserStr, err := getOrSetDefaultSetting(s, "guidance_user", "announcement")
	if err != nil {
		log.Fatalf("ガイダンスユーザー設定のロードに失敗しました: %v", err)
	}
	// カンマで分割し、各エントリの空白をトリムします
	var guidanceUsers []string
	if guidanceUserStr != "" {
		users := strings.Split(guidanceUserStr, ",")
		for _, u := range users {
			trimmed := strings.TrimSpace(u)
			if trimmed != "" {
				guidanceUsers = append(guidanceUsers, trimmed)
			}
		}
	}

	guidanceAudio, err := getOrSetDefaultSetting(s, "guidance_audio", "audio/announcement.wav")
	if err != nil {
		log.Fatalf("ガイダンス音声設定のロードに失敗しました: %v", err)
	}

	// SIPサーバーを作成
	sipServer := sip.NewSIPServer(s, *realm, guidanceUsers, guidanceAudio)

	// Webサーバーを作成
	webServer, err := web.NewServer(s, *realm, sipServer)
	if err != nil {
		log.Fatalf("Webサーバーの作成に失敗しました: %v", err)
	}

	// --- サーバーの実行 ---
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, gCtx := errgroup.WithContext(ctx)

	// Webサーバーを起動
	g.Go(func() error {
		log.Printf("Webサーバーを %s で起動しています", *webAddr)
		if err := webServer.Run(gCtx, *webAddr); err != nil {
			log.Printf("Webサーバーでエラーが発生しました: %v", err)
			return err
		}
		return nil
	})

	// SIPサーバーを起動
	g.Go(func() error {
		log.Printf("SIPサーバーを %s で起動しています", *sipAddr)
		if err := sipServer.Run(gCtx, *sipAddr); err != nil {
			log.Printf("SIPサーバーでエラーが発生しました: %v", err)
			return err
		}
		return nil
	})

	log.Println("アプリケーションが起動しました。Ctrl+Cで終了します。")

	// シャットダウンシグナルまたはサーバーのエラーを待機します
	if err := g.Wait(); err != nil {
		log.Printf("アプリケーションはエラーで終了しました: %v", err)
	} else {
		log.Println("アプリケーションは正常にシャットダウンしています。")
	}
}

// getOrSetDefaultSetting は、DBから設定を取得しようとします。
// 存在しない場合は、デフォルト値を設定して返します。
func getOrSetDefaultSetting(s *storage.Storage, key, defaultValue string) (string, error) {
	value, err := s.GetSetting(key)
	if err != nil {
		return "", err
	}
	if value == "" {
		log.Printf("設定 '%s' が見つかりません。デフォルト値 '%s' を設定します。", key, defaultValue)
		if err := s.SetSetting(key, defaultValue); err != nil {
			return "", err
		}
		return defaultValue, nil
	}
	log.Printf("設定 '%s' をDBからロードしました: '%s'", key, value)
	return value, nil
}
