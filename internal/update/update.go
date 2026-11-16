package update

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aist-dev/nostromo/internal/config"
)

// Release asset names. They are a contract with .goreleaser.yaml and install.sh.
const ChecksumsFile = "checksums.txt"

// ArchiveName is the release archive for a platform.
func ArchiveName(goos, goarch string) string {
	return "agent_" + goos + "_" + goarch + ".tar.gz"
}

// MaxDownload bounds a release download.
const MaxDownload = 200 << 20

// Restarter restarts the background service.
type Restarter interface {
	Restart(ctx context.Context) error
}

// Updater installs releases into a nostromo home.
type Updater struct {
	Paths        config.Paths
	Repo         string // owner/name
	APIBase      string // https://api.github.com
	DownloadBase string // https://github.com/<repo>/releases
	Token        string
	HTTP         *http.Client
	GOOS, GOARCH string
	Current      string
	Port         int
	Out          io.Writer
	Service      Restarter // nil: the server is not restarted
	// Health returns the version the local server reports.
	Health func(ctx context.Context, port int) (string, error)
	// RunBinary runs a binary and returns its stdout.
	RunBinary   func(ctx context.Context, path string, args ...string) (string, error)
	WaitTimeout time.Duration
}

func (u *Updater) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (u *Updater) get(ctx context.Context, url string, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", "nostromo-updater")
	if u.Token != "" && strings.HasPrefix(url, u.APIBase) {
		req.Header.Set("Authorization", "Bearer "+u.Token)
	}
	res, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, res.Status)
	}
	return res, nil
}

// Latest returns the tag of the latest published release.
func (u *Updater) Latest(ctx context.Context) (string, error) {
	res, err := u.get(ctx, u.APIBase+"/repos/"+u.Repo+"/releases/latest", "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("parse latest release: %w", err)
	}
	if _, err := ParseSemver(body.TagName); err != nil {
		return "", err
	}
	return body.TagName, nil
}

func (u *Updater) download(ctx context.Context, url string, limit int64) ([]byte, error) {
	res, err := u.get(ctx, url, "")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", url, limit)
	}
	return data, nil
}

// ExpectedChecksum finds the SHA-256 of file in a checksums.txt body.
func ExpectedChecksum(checksums []byte, file string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(string(checksums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == file {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s is not listed in %s", file, ChecksumsFile)
}

// ExtractBinary returns the "agent" file from a .tar.gz archive.
func ExtractBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("archive does not contain the agent binary")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && strings.TrimPrefix(h.Name, "./") == "agent" {
			return io.ReadAll(io.LimitReader(tr, MaxDownload))
		}
	}
}

// Options select what Run does.
type Options struct {
	Check    bool   // only report whether a newer version exists
	Version  string // install this version instead of the latest
	Rollback bool   // restore the previous binary
}

func (u *Updater) logf(format string, a ...any) { fmt.Fprintf(u.Out, format+"\n", a...) }

// Run performs an update according to o.
func (u *Updater) Run(ctx context.Context, o Options) error {
	if o.Rollback {
		return u.rollback(ctx, "rollback requested")
	}
	tag := o.Version
	if tag == "" {
		latest, err := u.Latest(ctx)
		if err != nil {
			return err
		}
		tag = latest
		if !Newer(tag, u.Current) {
			u.logf("already up to date (%s)", Normalize(u.Current))
			return nil
		}
	} else if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	if _, err := ParseSemver(tag); err != nil {
		return err
	}
	if o.Check {
		u.logf("version %s is available (installed %s); run: agent update", Normalize(tag), Normalize(u.Current))
		return nil
	}
	return u.install(ctx, tag)
}

func (u *Updater) install(ctx context.Context, tag string) error {
	want := Normalize(tag)
	file := ArchiveName(u.GOOS, u.GOARCH)
	base := u.DownloadBase + "/download/" + tag + "/"
	u.logf("downloading %s %s", file, tag)
	sums, err := u.download(ctx, base+ChecksumsFile, 1<<20)
	if err != nil {
		return err
	}
	expected, err := ExpectedChecksum(sums, file)
	if err != nil {
		return err
	}
	archive, err := u.download(ctx, base+file, MaxDownload)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != expected {
		return fmt.Errorf("checksum mismatch for %s (expected %s, got %s); nothing was changed", file, expected, got)
	}
	bin, err := ExtractBinary(archive)
	if err != nil {
		return err
	}
	newPath := u.Paths.NewBinary()
	if err := config.WriteFileAtomic(newPath, bin, 0o755); err != nil {
		return err
	}
	out, err := u.RunBinary(ctx, newPath, "version")
	if err != nil || Normalize(strings.TrimSpace(out)) != want {
		os.Remove(newPath)
		return fmt.Errorf("downloaded binary reports version %q, expected %s (%v); nothing was changed", strings.TrimSpace(out), want, err)
	}
	if err := copyFile(u.Paths.Binary(), u.Paths.PrevBinary()); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("back up the current binary: %w", err)
	}
	if err := os.Rename(newPath, u.Paths.Binary()); err != nil {
		return err
	}
	u.logf("installed %s", want)
	if out, err := u.RunBinary(ctx, u.Paths.Binary(), "setup", "--upgrade"); err != nil {
		u.logf("%s", out)
		return u.rollback(ctx, "setup --upgrade failed: "+err.Error())
	}
	if err := u.restartAndVerify(ctx, want); err != nil {
		return u.rollback(ctx, err.Error())
	}
	u.logf("updated to %s", want)
	return nil
}

func (u *Updater) restartAndVerify(ctx context.Context, want string) error {
	if u.Service == nil {
		return nil
	}
	if err := u.Service.Restart(ctx); err != nil {
		return fmt.Errorf("restart the service: %w", err)
	}
	timeout := u.WaitTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		v, err := u.Health(ctx, u.Port)
		if err == nil && (want == "" || Normalize(v) == want) {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("server did not come back: %w", err)
			}
			return fmt.Errorf("server reports version %s, expected %s", v, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (u *Updater) rollback(ctx context.Context, reason string) error {
	prev := u.Paths.PrevBinary()
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("%s; no previous version to roll back to", reason)
	}
	u.logf("rolling back: %s", reason)
	cur := u.Paths.Binary()
	tmp := cur + ".rollback"
	if err := copyFile(cur, tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := copyFile(prev, cur); err != nil {
		return err
	}
	if _, err := os.Stat(tmp); err == nil {
		os.Rename(tmp, prev)
	}
	version := ""
	if out, err := u.RunBinary(ctx, cur, "version"); err == nil {
		version = Normalize(strings.TrimSpace(out))
	}
	if err := u.restartAndVerify(ctx, version); err != nil {
		return fmt.Errorf("%s; rollback to %s did not come up either: %v", reason, version, err)
	}
	return fmt.Errorf("%s; rolled back to %s", reason, version)
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(dst, data, 0o755)
}

// ExecBinary runs a binary and returns its stdout.
func ExecBinary(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out) + string(ee.Stderr), err
		}
	}
	return string(out), err
}
