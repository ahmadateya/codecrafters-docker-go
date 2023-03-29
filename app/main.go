package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"syscall"
)

// Usage: your_docker.sh run <image> <command> <arg1> <arg2> ...
func main() {
	// get the command and its arguments
	command := os.Args[3]
	args := os.Args[4:len(os.Args)]
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

	// exec the command with chroot
	cmd := exec.Command(command, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// isolate the process
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID,
	}

	err = cmd.Run()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode := exitError.ProcessState.ExitCode()
			os.Exit(exitCode)
		} else {
			os.Exit(1)
		}
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
