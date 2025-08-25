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
	"syscall"
	"golang.org/x/sync/errgroup"
)

func main() {
	// --- 設定 ---
	var (
		webAddr = flag.String("web.addr", ":8080", "Web UIサーバーのアドレス")
		sipAddr = flag.String("sip.addr", ":5060", "SIPサーバーのアドレス")
		dbPath       = flag.String("db.path", "sip_users.db", "SQLiteデータベースファイルへのパス")
		realm        = flag.String("sip.realm", "go-sip-server", "認証用のSIPレルム")
		guidanceUser = flag.String("guidance.user", "announcement", "ガイダンスサービスをトリガーする特別なユーザー")
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

	// SIPサーバーを作成
	sipServer := sip.NewSIPServer(s, *realm, *guidanceUser)

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
		if err := webServer.Run(*webAddr); err != nil {
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
