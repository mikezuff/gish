package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// TODO: there are THREE different shell funcs. Consolidate and fix the docs.
func interactiveShellCmd(dir, cmd string, args ...string) error {
	c := exec.Command(cmd, args...)
	c.Dir = dir
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	err := c.Run()
	return err
}

/* Exec the given command connecting its IO to stdio. 
Stdout is copied to a buffer which is returned as a string.

TODO: This is meant to allow the user to authenticate with gitsvn. It should behave the same as git-svn by not echoing the password to the terminal. interactiveShellCmd behaves as expected, perhaps it could tee stdout??

Try 'stty -echo'

*/
func interactiveShellCmdToString(dir, arg0 string, args ...string) (string, error) {
	cmd := exec.Command(arg0, args...)
	cmd.Env = os.Environ()
	cmd.Dir = dir

	stdin, errin := cmd.StdinPipe()
	stdout, errout := cmd.StdoutPipe()
	stderr, errerr := cmd.StderrPipe()
	if errin != nil || errerr != nil || errout != nil {
		return "", fmt.Errorf("interactiveShell \"%s %v\" error on pipe: %s/%s/%s",
			arg0, args, errin, errout, errerr)
	}

	var b bytes.Buffer
	stdoutTee := io.TeeReader(stdout, &b)

	go io.Copy(stdin, os.Stdin)
	go io.Copy(os.Stdout, stdoutTee)
	go io.Copy(os.Stderr, stderr)

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("interactiveShell \"%s %v\" error on run: %s\n", arg0, args, err)
	}

	return b.String(), err
}

// Execute the given command and return the output.
func shellCmd(dir string, arg0 string, args ...string) (string, error) {
	cmd := exec.Command(arg0, args...)
	cmd.Env = os.Environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("shellCmd \"%s %v\" ERROR on pipe: %s",
			arg0, args, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("shellCmd \"%s %v\" ERROR on stderr pipe: %s",
			arg0, args, err)
	}

	err = cmd.Start()
	if err != nil {
		return "", fmt.Errorf("shellCmd \"%s %v\" ERROR on start: %s",
			arg0, args, err)
	}

	var b bytes.Buffer
	_, err = b.ReadFrom(stdout)
	if err != nil {
		return "", fmt.Errorf("shellCmd \"%s %v\" ERROR on read: %s",
			arg0, args, err)
	}

	var errBuf bytes.Buffer
	_, err = errBuf.ReadFrom(stderr)
	if err != nil {
		return "", fmt.Errorf("shellCmd \"%s %v\" ERROR on stderr read: %s",
			arg0, args, err)
	}

	err = cmd.Wait()
	if err != nil {
		fmt.Fprintf(os.Stderr, errBuf.String())
		return "", fmt.Errorf("shellCmd \"%s %v\" ERROR on wait: %s",
			arg0, args, err)

	}

	return b.String(), nil
}
