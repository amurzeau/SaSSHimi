// Copyright © 2018 Raul Sampedro
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"errors"
	"fmt"
	"github.com/rsrdesarrollo/SaSSHimi/common"
	"github.com/rsrdesarrollo/SaSSHimi/utils"
	"github.com/spf13/viper"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	user2 "os/user"
	"strings"
	"sync"
	"syscall"
	"time"
)

type tunnel struct {
	common.ChannelForwarder
	sshClient      *ssh.Client
	sshSession     *ssh.Session
	viper          *viper.Viper
	transparentCmd []string
}

func newTransparentTunnel(transparentCmd []string) *tunnel {
	return &tunnel{
		ChannelForwarder: common.ChannelForwarder{
			OutChannel: make(chan *common.DataMessage, 10),
			InChannel:  make(chan *common.DataMessage, 10),

			ChannelOpen: true,
			ClientsLock: &sync.Mutex{},
			Clients:     make(map[string]*common.Client),

			NotifyClosure: make(chan struct{}),
		},
		transparentCmd: transparentCmd,
	}
}

func newTunnel(viper *viper.Viper) *tunnel {
	return &tunnel{
		ChannelForwarder: common.ChannelForwarder{
			OutChannel: make(chan *common.DataMessage, 10),
			InChannel:  make(chan *common.DataMessage, 10),

			ChannelOpen: true,
			ClientsLock: &sync.Mutex{},
			Clients:     make(map[string]*common.Client),

			NotifyClosure: make(chan struct{}),
		},
		viper: viper,
	}
}

func (t *tunnel) getRemoteHost() string {
	remoteHost := t.viper.GetString("RemoteHost")
	if !strings.Contains(remoteHost, ":") {
		remoteHost = remoteHost + ":22"
	}

	utils.Logger.Debug("SSH Remote Host:", remoteHost)
	return remoteHost
}

func (t *tunnel) getUsername() string {
	user := t.viper.GetString("User")
	if user == "" {
		user, _ := user2.Current()
		return user.Name
	}
	utils.Logger.Debug("SSH User:", user)
	return user
}

func (t *tunnel) getRemoteExecutable() string {
	remoteExecutable := t.viper.GetString("RemoteExecutable")
	if remoteExecutable == "" {
		remoteExecutable, _ = os.Executable()
	}
	utils.Logger.Debug("Remote Executable:", remoteExecutable)
	return remoteExecutable
}

func (t *tunnel) getRemoteAgentPath() string {
	remoteAgentPath := t.viper.GetString("RemoteAgentPath")
	if remoteAgentPath == "" {
		remoteAgentPath = "."
	}
	utils.Logger.Debug("Remote install path:", remoteAgentPath)
	return remoteAgentPath
}

func (t *tunnel) getPassword() string {
	password := t.viper.GetString("Password")
	if password == "" {
		fmt.Printf("%s@%s's password: ", t.getUsername(), t.getRemoteHost())
		bytePassword, _ := terminal.ReadPassword(int(syscall.Stdin))
		fmt.Println("")
		password = string(bytePassword)
	}
	return password
}

func (t *tunnel) getPublicKey() ssh.Signer {
	pkFilePath := t.viper.GetString("PrivateKey")

	if pkFilePath == "" {
		return nil
	}

	key, err := ioutil.ReadFile(pkFilePath)
	if err != nil {
		utils.Logger.Fatalf("unable to read private key: %v", err)
	}

	// Create the Signer for this private key.
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		utils.Logger.Fatalf("unable to parse private key: %v", err)
	}

	return signer
}

func (t *tunnel) uploadForwarder(remoteAgentPath string) error {
	session, err := t.sshClient.NewSession()
	defer session.Close()
	if err != nil {
		return errors.New("Failed to create session: " + err.Error())
	}

	var remoteExecutable string = t.getRemoteExecutable()

	selfFile, err := os.Open(remoteExecutable)
	session.Stdin = selfFile

	if err != nil {
		return errors.New("Failed to open current binary " + err.Error())
	}

	remoteAgentPathEscaped := utils.EscapeBashArgument(remoteAgentPath)
	command := fmt.Sprintf("cd %s && cat > ./.daemon && chmod +x ./.daemon", remoteAgentPathEscaped)
	err = session.Run(command)

	return err
}

func (t *tunnel) openTransparentTunnel() error {
	var err error

	cmd := exec.Command(t.transparentCmd[0], t.transparentCmd[1:]...)

	t.Writer, _ = cmd.StdinPipe()
	t.Reader, _ = cmd.StdoutPipe()

	cmd.Stderr = os.Stderr

	go t.ReadInputData()
	go t.WriteOutputData()


	utils.Logger.Notice("Transparent Tunnel Opening")

	err = cmd.Run()

	if err != nil {
		return errors.New("Run transparent command error: " + err.Error())
	}

	t.ChannelOpen = false
	t.NotifyClosure <- struct{}{}

	return errors.New("Remote process is dead")
}

func (t *tunnel) openTunnel(verboseLevel int) error {
	var err error

	var authMethods = []ssh.AuthMethod{}

	pkSigner := t.getPublicKey()
	if pkSigner != nil {
		authMethods = append(authMethods, ssh.PublicKeys(pkSigner))
	}
	authMethods = append(authMethods, ssh.Password(t.getPassword()))

	config := &ssh.ClientConfig{
		User:            t.getUsername(),
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Auth:            authMethods,
	}

	t.sshClient, err = ssh.Dial("tcp", t.getRemoteHost(), config)

	if err != nil {
		return errors.New("Dial error: " + err.Error())
	}

	defer t.sshClient.Close()

	remoteAgentPath := t.getRemoteAgentPath()
	err = t.uploadForwarder(remoteAgentPath)
	if err != nil {
		return errors.New("Failed to upload forwarder " + err.Error())
	}

	t.sshSession, err = t.sshClient.NewSession()
	defer t.sshSession.Close()

	if err != nil {
		return errors.New("Failed to create session: " + err.Error())
	}

	t.Writer, err = t.sshSession.StdinPipe()
	if err != nil {
		return errors.New("Failed to pipe STDIN on session: " + err.Error())
	}

	t.Reader, err = t.sshSession.StdoutPipe()
	if err != nil {
		return errors.New("Failed to pipe STDOUT on session: " + err.Error())
	}

	t.sshSession.Stderr = os.Stderr

	go t.ReadInputData()
	go t.WriteOutputData()

	utils.Logger.Notice("SSH Tunnel Open")

	var commandOps = ""

	if verboseLevel != 0 {
		commandOps = "-" + strings.Repeat("v", verboseLevel)
	}

	remoteAgentPathEscaped := utils.EscapeBashArgument(remoteAgentPath)
	var runCommand = fmt.Sprintf("cd %s && ./.daemon agent %s", remoteAgentPathEscaped, commandOps)
	t.sshSession.Run(runCommand)

	t.ChannelOpen = false
	t.NotifyClosure <- struct{}{}

	return errors.New("Remote process is dead")
}

func (t *tunnel) handleClients() {
	for t.ChannelOpen {
		msg := <-t.InChannel

		if msg.KeepAlive {
			continue
		}

		t.ClientsLock.Lock()

		client, prs := t.Clients[msg.ClientId]

		if prs == false {
			utils.Logger.Warning("Received data from closed client", msg.ClientId)
		} else {
			if msg.DeadClient {
				// ACK for client termination
				client.NotifyEOF(false)
				client.Terminate()
				delete(t.Clients, msg.ClientId)
			} else if msg.CloseClient {
				client.Close()
				delete(t.Clients, msg.ClientId)
			} else if !client.IsDead() {
				err := client.Write(msg.Data)

				if err != nil {
					client.Terminate()
					client.NotifyEOF(true)

					utils.Logger.Errorf("Error Writing: %s\n", err.Error())
				}

			}
		}

		t.ClientsLock.Unlock()
	}
}

func RunTransparent(transparentCmd []string, bindAddress string) {
	ln, err := net.Listen("tcp", bindAddress)

	if err != nil {
		panic("Failed to bind local port " + err.Error())
	}

	utils.Logger.Notice("Proxy bind at", bindAddress)

	tunnel := newTransparentTunnel(transparentCmd)

	go func() {
		err = tunnel.openTransparentTunnel()

		if err != nil {
			utils.Logger.Fatal("Failed to open tunnel ", err.Error())
		}
	}()

	go tunnel.handleClients()
	go tunnel.KeepAlive()

	for tunnel.ChannelOpen {
		conn, err := ln.Accept()
		if err != nil {
			utils.Logger.Fatalf("Error in connection accept: %s", err.Error())
			continue
		}

		utils.Logger.Debug("New connection from ", conn.RemoteAddr().String())

		client := common.NewClient(
			conn.RemoteAddr().String(),
			conn,
			tunnel.OutChannel,
		)

		tunnel.Clients[client.Id] = client
		go client.ReadFromClientToChannel()
	}
}

func Run(viper *viper.Viper, bindAddress string, verboseLevel int) {

	ln, err := net.Listen("tcp", bindAddress)

	if err != nil {
		panic("Failed to bind local port " + err.Error())
	}

	utils.Logger.Notice("Proxy bind at", bindAddress)

	tunnel := newTunnel(viper)

	termios := TermiosSaveStdin()
	onExit := func() {
		TermiosRestoreStdin(termios)
		tunnel.Terminate()

		utils.Logger.Notice("Waiting to remote process to clean up...")
		select {
		case <-tunnel.NotifyClosure:
		case <-time.After(5 * time.Second):
			tunnel.sshSession.Signal(ssh.SIGTERM)
			utils.Logger.Warning("Remote close timeout. Sending TERM signal.")
		}

		select {
		case <-tunnel.NotifyClosure:
		case <-time.After(5 * time.Second):
			utils.Logger.Error("Remote process don't respond. Force close channel.")
			utils.Logger.Error("IMPORTANT: This might leave files in remote host.")
			tunnel.sshSession.Close()
		}

		tunnel.sshClient.Close()
		ln.Close()
	}

	utils.ExitCallback(onExit)

	go func() {
		err = tunnel.openTunnel(verboseLevel)

		if err != nil {
			utils.Logger.Fatal("Failed to open tunnel ", err.Error())
		}
	}()

	go tunnel.handleClients()
	go tunnel.KeepAlive()

	for tunnel.ChannelOpen {
		conn, err := ln.Accept()
		if err != nil {
			utils.Logger.Fatalf("Error in conncetion accept: %s", err.Error())
			continue
		}

		utils.Logger.Debug("New connection from ", conn.RemoteAddr().String())

		client := common.NewClient(
			conn.RemoteAddr().String(),
			conn,
			tunnel.OutChannel,
		)

		tunnel.Clients[client.Id] = client
		go client.ReadFromClientToChannel()
	}
}
