package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const registryApiBaseURL = "https://registry-1.docker.io"

type nullReader struct{}

func (nullReader) Read(p []byte) (n int, err error) { return len(p), nil }

type FsLayer struct {
	BlobSum string `json:"blobSum"`
}

type Manifest struct {
	Name     string    `json:"name"`
	Tag      string    `json:"tag"`
	FsLayers []FsLayer `json:"fsLayers"`
}

func copyBinary(rootDir string, binPath string) error {
	if err := os.MkdirAll(filepath.Join(rootDir, filepath.Dir(binPath)), os.ModePerm); err != nil {
		return err
	}

	dstFd, err := os.OpenFile(filepath.Join(rootDir, binPath), os.O_RDWR|os.O_CREATE, 0777)
	if err != nil {
		return err
	}
	defer dstFd.Close()

	srcFd, err := os.Open(binPath)
	if err != nil {
		return err
	}
	defer srcFd.Close()

	if _, err := io.Copy(dstFd, srcFd); err != nil {
		return err
	}

	return nil
}

func chroot(rootfs string, binPath string) error {
	if err := os.Chdir(rootfs); err != nil {
		return err
	}

	if err := syscall.Chroot(rootfs); err != nil {
		return err
	}

	return nil
}

func fetchToken(name string) (string, error) {
	u, err := url.Parse("https://auth.docker.io/token")
	if err != nil {
		return "", err
	}

	params := url.Values{}
	params.Add("scope", fmt.Sprintf("repository:library/%s:pull", name))
	params.Add("service", "registry.docker.io")

	u.RawQuery = params.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var auth struct {
		Token string
	}
	if err := json.NewDecoder(resp.Body).Decode(&auth); err != nil {
		return "", err
	}

	return auth.Token, nil
}

func fetchManifest(name string, token string) (*Manifest, error) {
	u, err := url.JoinPath(registryApiBaseURL, "v2", "library", name, "manifests", "latest")
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v1json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var manifest *Manifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return nil, err
	}

	return manifest, nil
}

func pullLayer(rootfs string, name string, digest string, token string) error {
	u, err := url.JoinPath(registryApiBaseURL, "v2", "library", name, "blobs", digest)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	tarName := filepath.Join(rootfs, fmt.Sprintf("%s.tar.gz", digest))
	tarFd, err := os.OpenFile(
		tarName,
		os.O_RDWR|os.O_CREATE,
		0777,
	)
	if err != nil {
		return err
	}

	if _, err := io.Copy(tarFd, resp.Body); err != nil {
		tarFd.Close()
		return err
	}
	tarFd.Close()

	cmd := exec.Command("tar", "-xzf", tarName, "-C", rootfs)
	if err := cmd.Run(); err != nil {
		return err
	}

	if err := os.Remove(tarName); err != nil {
		return err
	}

	return nil
}

func pullImage(rootfs string, name string) error {
	token, err := fetchToken(name)
	if err != nil {
		return err
	}

	manifest, err := fetchManifest(name, token)
	if err != nil {
		return err
	}

	for _, fsLayer := range manifest.FsLayers {
		if err := pullLayer(rootfs, name, fsLayer.BlobSum, token); err != nil {
			return err
		}
	}

	return nil
}

// Usage: your_docker.sh run <image> <command> <arg1> <arg2> ...
func main() {
	tmpDir, err := os.MkdirTemp("", "jail")
	if err != nil {
		log.Fatalln(err)
	}
	defer os.RemoveAll(tmpDir)

	image := os.Args[2]
	var imageName string
	if strings.Contains(image, ":") {
		imageName = strings.Split(image, ":")[0]
	} else {
		imageName = image
	}

	command := os.Args[3]
	if err := copyBinary(tmpDir, command); err != nil {
		log.Fatalln(err)
	}

	if err := pullImage(tmpDir, imageName); err != nil {
		log.Fatalln(err)
	}
	args := os.Args[4:len(os.Args)]

	// create a chroot directory
	if err := chroot(tmpDir, command); err != nil {
		log.Fatalln(err)
	}

	cmd := exec.Command(command, args...)
	cmd.Stdin = nullReader{}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID,
	}
	if err := cmd.Run(); err != nil {
		os.Exit(cmd.ProcessState.ExitCode())
	}
}
