package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/joho/godotenv"
	"github.com/minio/minio-go/v7"
	miniocredentials "github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pkg/sftp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/ssh"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	metricsAddr      = "0.0.0.0:2112"
	promNS           = "dtd2mysql_runner"
	intervalDuration = 24 * time.Hour
	serverName       = "dtd2mysql"
)

var (
	lastDownloadGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: promNS,
		Name:      "latest_timetable_downloaded_at_unix_gauge",
		Help:      "The time the latest timetable processed was downloaded from national rail in unix seconds",
	})
	runtimeGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: promNS,
		Name:      "runtime_minutes_gauge",
		Help:      "The duration of the last run in minutes",
	})
)

const (
	serverTypeName = "cax21"
	sshKeyName     = "dtd2mysql-runner"
	locationName   = "nbg1"
	outputBucket   = "nationalrail-gtfs"
	outputName     = "timetable.gtfs.zip"
)

func main() {
	if err := godotenv.Load(".env.local"); err != nil {
		fmt.Println("No .env.local")
	}

	snapshotID, err := strconv.ParseInt(mustGetEnv("SNAPSHOT_ID"), 10, 64)
	if err != nil {
		log.Fatal("Invalid SNAPSHOT_ID")
	}

	hcloudToken := mustGetEnv("HCLOUD_TOKEN")

	sshPrivateKeyBytes, err := base64.StdEncoding.DecodeString(mustGetEnv("SSH_PRIVATE_KEY_BASE64"))
	if err != nil {
		log.Fatal("Invalid SSH_PRIVATE_KEY_BASE64")
	}
	sshSigner, err := ssh.ParsePrivateKey(sshPrivateKeyBytes)
	if err != nil {
		log.Fatal("Invalid SSH_PRIVATE_KEY_BASE64")
	}

	nationalRailUsername := mustGetEnv("NATIONAL_RAIL_USERNAME")
	nationalRailPassword := mustGetEnv("NATIONAL_RAIL_PASSWORD")

	minioAccessKey := mustGetEnv("MINIO_ACCESS_KEY")
	minioSecretKey := mustGetEnv("MINIO_SECRET_KEY")

	http.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, "OK")
	})
	http.Handle("GET /metrics", promhttp.Handler())
	go func() {
		fmt.Println("Metrics server listening on", "http://"+metricsAddr)
		err = http.ListenAndServe(metricsAddr, nil)
		if err != nil {
			slog.Error("Failed to serve metrics", "error", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		fmt.Println("Received", sig)
		cancel()
	}()

	for {
		err = run(
			ctx,
			snapshotID,
			hcloudToken,
			sshSigner,
			nationalRailUsername,
			nationalRailPassword,
			minioAccessKey,
			minioSecretKey,
		)
		if errors.Is(err, context.Canceled) {
			break
		} else if err != nil {
			slog.Error("Failed to run", "error", err)
		}

		sleepUntil := time.Now().Add(intervalDuration)
		fmt.Println("Sleeping until", sleepUntil)
		timer := time.NewTimer(intervalDuration)
		select {
		case <-ctx.Done():
			fmt.Println("Cancelling sleep")
			break
		case <-timer.C:
			fmt.Println("Done sleeping")
			continue
		}
	}

	fmt.Println("All done")
}

func run(
	ctx context.Context,
	snapshotID int64,
	hcloudToken string,
	sshSigner ssh.Signer,
	nationalRailUsername, nationalRailPassword string,
	minioAccessKey, minioSecretKey string,
) error {
	startTime := time.Now()

	hc := hcloud.NewClient(hcloud.WithToken(hcloudToken))

	mc, err := minio.New("minio.dfranklin.dev", &minio.Options{
		Creds:  miniocredentials.NewStaticV4(minioAccessKey, minioSecretKey, ""),
		Secure: true,
	})
	if err != nil {
		return err
	}

	srv, err := createServer(ctx, hc, snapshotID)
	if err != nil {
		return err
	}

	defer func() {
		res, _, err := hc.Server.DeleteWithResult(context.Background(), srv)
		if err != nil {
			slog.Error("Failed to delete server", "error", err)
		}
		fmt.Println("Submitted server delete request")
		err = hc.Action.WaitFor(context.Background(), res.Action)
		if err != nil {
			slog.Error("Failed to wait for server deletion", "error", err)
		}
		fmt.Println("Server deleted")
	}()

	sshClient, err := dialServer(ctx, srv, sshSigner)
	if err != nil {
		return err
	}
	defer func(sshClient *ssh.Client) {
		if err := sshClient.Close(); err != nil {
			slog.Error("Failed to close ssh client", "error", err)
		}
	}(sshClient)
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		return err
	}

	downloadTime := time.Now()
	err = fetchInput(ctx, sftpClient, nationalRailUsername, nationalRailPassword)
	if err != nil {
		return err
	}

	err = runEntrypoint(ctx, sshClient)
	if err != nil {
		return err
	}

	err = uploadOutput(ctx, sftpClient, mc)
	if err != nil {
		return err
	}

	lastDownloadGauge.Set(float64(downloadTime.Unix()))
	runtimeGauge.Set(float64(time.Since(startTime)) / float64(time.Minute))

	return nil
}

func createServer(ctx context.Context, hc *hcloud.Client, snapshotID int64) (*hcloud.Server, error) {
	prevSrv, _, err := hc.Server.GetByName(ctx, serverName)
	if err == nil && prevSrv != nil {
		_, _, _ = hc.Server.DeleteWithResult(ctx, prevSrv)
	}

	img, _, err := hc.Image.GetByID(context.Background(), snapshotID)
	if err != nil {
		return nil, err
	}
	if img == nil {
		return nil, fmt.Errorf("snapshot %d not found", snapshotID)
	}
	fmt.Printf("Using image %s (%d)\n", img.Description, img.ID)

	serverType, _, err := hc.ServerType.GetByName(ctx, serverTypeName)
	if err != nil {
		return nil, err
	}
	if serverType == nil {
		return nil, fmt.Errorf("server type %s not found", serverTypeName)
	}
	fmt.Printf("Using server type %+v\n", serverType)

	sshKey, _, err := hc.SSHKey.GetByName(ctx, sshKeyName)
	if err != nil {
		return nil, err
	}
	if sshKey == nil {
		return nil, fmt.Errorf("ssh key with name %s not found", sshKeyName)
	}
	fmt.Printf("Using ssh key %s (%s)\n", sshKey.Name, sshKey.Fingerprint)

	loc, _, err := hc.Location.GetByName(ctx, locationName)
	if err != nil {
		return nil, err
	}
	if loc == nil {
		return nil, fmt.Errorf("location %s not found", locationName)
	}
	fmt.Printf("Using location %+v\n", loc)

	fmt.Println("Creating server")
	srv, _, err := hc.Server.Create(ctx, hcloud.ServerCreateOpts{
		// Note that because Hetzner enforces unique server names a runaway script won't
		// be able to create multiple servers
		Name:       serverName,
		ServerType: serverType,
		Image:      img,
		SSHKeys:    []*hcloud.SSHKey{sshKey},
		Location:   loc,
	})
	if err != nil {
		return nil, err
	}
	fmt.Println("Submitted server create request")

	err = hc.Action.WaitFor(ctx, srv.NextActions...)
	if err != nil {
		return nil, fmt.Errorf("wait for server create next actions: %w", err)
	}

	return srv.Server, nil
}

func dialServer(ctx context.Context, srv *hcloud.Server, sshSigner ssh.Signer) (*ssh.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute*15)
	defer cancel()

	addr := srv.PublicNet.IPv4.IP.String() + ":22"

	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(sshSigner)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	fmt.Println("Dialing server")
	for {
		time.Sleep(10 * time.Second)
		client, err := ssh.Dial("tcp", addr, cfg)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		} else if err != nil {
			fmt.Printf("Failed to dial server: %s\n", err)
			continue
		} else {
			fmt.Println("Dialed server")
			return client, nil
		}
	}
}

func fetchInput(ctx context.Context, sftpClient *sftp.Client, nationalRailUsername, nationalRailPassword string) error {
	httpClient := &http.Client{}

	authValues := url.Values{}
	authValues.Set("username", nationalRailUsername)
	authValues.Set("password", nationalRailPassword)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://opendata.nationalrail.co.uk/authenticate", strings.NewReader(authValues.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("authenticate status %s: %s", resp.Status, string(body))
	}
	var authResp struct {
		Token string `json:"token"`
	}
	err = json.Unmarshal(body, &authResp)
	if err != nil {
		return err
	}
	fmt.Println("Authenticated with national rail")

	req, err = http.NewRequestWithContext(ctx, "GET", "https://opendata.nationalrail.co.uk/api/staticfeeds/3.0/timetable", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Auth-Token", authResp.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("get timetable status %s: %s", resp.Status, string(body))
	}

	out, err := sftpClient.Create("/data/input.cif.zip")
	if err != nil {
		return err
	}
	defer func(out *sftp.File) {
		_ = out.Close()
	}(out)
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}
	err = out.Close()
	if err != nil {
		return err
	}

	fmt.Println("Copied timetable to server")
	return nil
}

func runEntrypoint(ctx context.Context, sshClient *ssh.Client) error {
	fmt.Println("Running entrypoint")

	sess, err := sshClient.NewSession()
	if err != nil {
		return err
	}

	var outputWriter singleWriter
	sess.Stdout = &outputWriter
	sess.Stderr = &outputWriter

	err = sess.Start("/root/entrypoint.sh")
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		err := sess.Signal(ssh.SIGKILL)
		if err != nil && !errors.Is(err, io.EOF) {
			slog.Error("Failed to kill session", "error", err)
		}
	}()

	err = sess.Wait()
	lines := outputWriter.Lines()
	for lines.Scan() {
		fmt.Println("[server]  ", lines.Text())
	}
	if err != nil {
		return fmt.Errorf("command failed: %w", err)
	}

	fmt.Println("Ran entrypoint")
	return nil
}

func uploadOutput(ctx context.Context, sftpClient *sftp.Client, mc *minio.Client) error {
	fmt.Println("Uploading output")

	f, err := sftpClient.Open("/data/output.gtfs.zip")
	if err != nil {
		return err
	}
	stat, err := f.Stat()
	if err != nil {
		return err
	}

	_, err = mc.PutObject(ctx, outputBucket, outputName, f, stat.Size(), minio.PutObjectOptions{})
	if err != nil {
		return err
	}

	fmt.Printf("Uploaded %s/%s\n", outputBucket, outputName)
	return nil
}

func mustGetEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatal("Missing environment variable " + k)
	}
	return v
}

type singleWriter struct {
	b  bytes.Buffer
	mu sync.Mutex
}

func (w *singleWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *singleWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Bytes()
}

func (w *singleWriter) Lines() *bufio.Scanner {
	b := w.Bytes()
	return bufio.NewScanner(bytes.NewReader(b))
}
