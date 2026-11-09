// Package javaserver runs the stock Java Edition server that holds the shared world: it fetches the official 1.8.9 server jar
// on first start (not redistributed in the image: the Minecraft EULA forbids it), writes its properties and keeps it running.
package javaserver

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	jarURL  = "https://launcher.mojang.com/v1/objects/b58b2ceb36e01bcd8dbf49c8fb66c55a9f0676cd/server.jar" // 1.8.9
	jarSHA1 = "b58b2ceb36e01bcd8dbf49c8fb66c55a9f0676cd"
)

// Config for the server process.
type Config struct {
	Dir          string // working directory and world volume
	Java         string // java binary
	Xmx          string
	Port         int
	ViewDistance int
	Motd         string
	NativeEpoll  bool // the 1.8.9 netty epoll transport fails on Java 17 and later; the container's Java 8 can use it
}

func (c Config) properties() string {
	return fmt.Sprintf(`online-mode=false
server-port=%d
view-distance=%d
spawn-protection=0
motd=%s
max-players=16
level-name=world
gamemode=0
difficulty=1
enable-command-block=false
use-native-transport=%v
`, c.Port, c.ViewDistance, c.Motd, c.NativeEpoll)
}

func ensureJar(dir string) (string, error) {
	jar := filepath.Join(dir, "server.jar")
	if b, err := os.ReadFile(jar); err == nil {
		if s := sha1.Sum(b); hex.EncodeToString(s[:]) == jarSHA1 {
			return jar, nil
		}
	}
	slog.Info("fetching the 1.8.9 server jar", "url", jarURL)
	resp, err := http.Get(jarURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", err
	}
	if s := sha1.Sum(b); hex.EncodeToString(s[:]) != jarSHA1 {
		return "", fmt.Errorf("server jar sha1 mismatch")
	}
	return jar, os.WriteFile(jar, b, 0o644)
}

// Run keeps the server up until ctx ends, restarting it after an exit. It returns once the first start has been attempted; the
// process runs in the background.
func Run(ctx context.Context, c Config) error {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	jar, err := ensureJar(c.Dir)
	if err != nil {
		return err
	}
	// the owner of this service accepts Mojang's EULA by running the server (https://aka.ms/MinecraftEULA)
	if err := os.WriteFile(filepath.Join(c.Dir, "eula.txt"), []byte("eula=true\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "server.properties"), []byte(c.properties()), 0o644); err != nil {
		return err
	}
	go func() {
		for ctx.Err() == nil {
			cmd := exec.CommandContext(ctx, c.Java, "-Xmx"+c.Xmx, "-Xms128m", "-jar", jar, "nogui")
			cmd.Dir = c.Dir
			out, _ := cmd.StdoutPipe()
			cmd.Stderr = cmd.Stdout
			if err := cmd.Start(); err != nil {
				slog.Error("java server start", "err", err)
				time.Sleep(10 * time.Second)
				continue
			}
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				slog.Info("java", "line", sc.Text())
			}
			err := cmd.Wait()
			slog.Warn("java server exited", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}()
	return nil
}
