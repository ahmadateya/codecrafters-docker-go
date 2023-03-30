package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"syscall"
	"time"
)

var registry = "https://registry-1.docker.io/v2/library"

type apiTOKEN struct {
	Token       string    `json:"token"`
	AccessToken string    `json:"access_token"`
	ExpiresIn   int       `json:"expires_in"`
	IssuedAt    time.Time `json:"issued_at"`
}

type Manifest struct {
	Name     string    `json:"name"`
	Tag      string    `json:"tag"`
	FsLayers []FsLayer `json:"fsLayers"`
}

type FsLayer struct {
	BlobSum string `json:"blobSum"`
}

// Usage: your_docker.sh run <image> <command> <arg1> <arg2> ...
func main() {
	// get the command and its arguments
	image := os.Args[2]
	command := os.Args[3]
	args := os.Args[4:len(os.Args)]

	// isolate file system
	chrootDir := isolateFileSystem(command)

	// pull and deal with docker image
	pullDockerImage(image, chrootDir)

	// exec the command with chroot
	cmd := exec.Command(command, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// isolate the process; learn more https://youtu.be/sK5i-N34im8
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID,
	}

	// run the command
	err := cmd.Run()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode := exitError.ProcessState.ExitCode()
			os.Exit(exitCode)
		} else {
			os.Exit(1)
		}
	}
}

func isolateFileSystem(command string) string {
	// create a chroot directory
	chrootDir, err := os.MkdirTemp("", "")
	if err != nil {
		fmt.Printf("error creating chroot dir: %v", err)
		os.Exit(1)
	}

	if err = copyExecutableIntoDir(chrootDir, command); err != nil {
		fmt.Printf("error copying executable into chroot dir: %v", err)
		os.Exit(1)
	}

	// Create /dev/null so that cmd.Run() doesn't complain;
	// here is an explanation https://rohitpaulk.com/articles/cmd-run-dev-null
	if err = createDevNull(chrootDir); err != nil {
		fmt.Printf("error creating /dev/null: %v", err)
		os.Exit(1)
	}

	// using chroot syscall on the chrootDir
	if err = syscall.Chroot(chrootDir); err != nil {
		fmt.Printf("chroot err: %v", err)
		os.Exit(1)
	}
	return chrootDir
}

func pullDockerImage(image, chrootDir string) {
	// authenticate with docker.io
	token, err := getBearerToken(image)
	if err != nil {
		fmt.Printf("error getting token: %v", err)
		os.Exit(1)
	}

	manifest, err := fetchManifest(token, image)
	if err != nil {
		fmt.Printf("error fetching manifest: %v", err)
		os.Exit(1)
	}

	if err := extractImage(chrootDir, token, image, manifest); err != nil {
		fmt.Printf("error extracting image: %v", err)
		os.Exit(1)
	}
}

// executablePath => /usr/local/bin/docker-explorer
// chrootDir => /tmp/*
func copyExecutableIntoDir(chrootDir string, executablePath string) error {
	executablePathInChrootDir := path.Join(chrootDir, executablePath)

	if err := os.MkdirAll(path.Dir(executablePathInChrootDir), 0750); err != nil {
		return err
	}

	return copyFile(executablePath, executablePathInChrootDir)
}

func copyFile(sourceFilePath, destinationFilePath string) error {
	sourceFileStat, err := os.Stat(sourceFilePath)
	if err != nil {
		return err
	}

	sourceFile, err := os.Open(sourceFilePath)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destinationFile, err := os.OpenFile(destinationFilePath, os.O_RDWR|os.O_CREATE, sourceFileStat.Mode())
	if err != nil {
		return err
	}
	defer destinationFile.Close()

	_, err = io.Copy(destinationFile, sourceFile)
	return err
}

func createDevNull(chrootDir string) error {
	if err := os.Mkdir(path.Join(chrootDir, "dev"), os.ModePerm); err != nil {
		return err
	}
	return os.WriteFile(path.Join(chrootDir, "dev", "null"), []byte{}, 0644)
}

func getBearerToken(repository string) (string, error) {
	var apiResponse apiTOKEN

	service := "registry.docker.io"

	response, err := http.Get(fmt.Sprintf(`https://auth.docker.io/token?service=%s&scope=repository:library/%s:pull`, service, repository))
	if err != nil {
		return "", fmt.Errorf("failed to call https://auth.docker.io/token: %w", err)
	}
	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read http response body: %w", err)
	}

	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return "", fmt.Errorf("failed to parse http response: %w", err)
	}

	return apiResponse.Token, nil
}
func fetchManifest(token string, image string) (*Manifest, error) {
	imageManifestURL := fmt.Sprintf("%s/%s/manifests/latest", registry, image)
	imageManifestReq, _ := http.NewRequest("GET", imageManifestURL, nil)
	imageManifestReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	imageManifestReq.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v1json")
	imageManifestRes, err := http.DefaultClient.Do(imageManifestReq)
	if err != nil {
		return nil, err
	}
	defer imageManifestRes.Body.Close()
	body, err := ioutil.ReadAll(imageManifestRes.Body)
	if err != nil {
		return nil, err
	}

	var manifest Manifest
	return &manifest, json.Unmarshal(body, &manifest)
}

func extractImage(rootDir, token, repository string, manifest *Manifest) error {
	for index, digest := range manifest.FsLayers {
		if err := fetchLayer(rootDir, token, repository, digest, index); err != nil {
			return err
		}
	}

	return nil
}

func fetchLayer(rootDir, token, repository string, fsLayer FsLayer, index int) error {
	var response *http.Response

	url := fmt.Sprintf("%s/%s/blobs/%s", registry, repository, fsLayer.BlobSum)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to read http response body: %w", err)
	}
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", token))

	response, err = http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to read http response body: %w", err)
	}
	defer response.Body.Close()

	// Temporary Redirect
	if response.StatusCode == 307 {
		redirectUrl := response.Header.Get("location")
		req, err := http.NewRequest(http.MethodGet, redirectUrl, nil)
		if err != nil {
			return fmt.Errorf("failed to read http response body: %w", err)
		}
		req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", token))

		response, err = http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("failed to read http response body: %w", err)
		}
		defer response.Body.Close()
	}

	data, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}

	tarball := fmt.Sprintf("%s.tar", fsLayer.BlobSum)
	if err := ioutil.WriteFile(tarball, data, 0644); err != nil {
		return err
	}
	defer os.Remove(tarball)

	cmd := exec.Command("tar", "xpf", tarball, "-C", rootDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
