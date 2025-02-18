/*
Copyright 2022 AmidaWare LLC.

Licensed under the Tactical RMM License Version 1.0 (the “License”).
You may only use the Licensed Software in accordance with the License.
A copy of the License is available at:

https://license.tacticalrmm.com

*/

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"time"

	rmm "github.com/amidaware/rmmagent/shared"
	ps "github.com/elastic/go-sysinfo"
	gocmd "github.com/go-cmd/cmd"
	"github.com/go-resty/resty/v2"
	"github.com/kardianos/service"
	nats "github.com/nats-io/nats.go"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/sirupsen/logrus"
	trmm "github.com/wh1te909/trmm-shared"
)

// Agent struct
type Agent struct {
	Hostname      string
	Arch          string
	AgentID       string
	BaseURL       string
	ApiURL        string
	Token         string
	AgentPK       int
	Cert          string
	ProgramDir    string
	EXE           string
	SystemDrive   string
	MeshInstaller string
	MeshSystemEXE string
	MeshSVC       string
	PyBin         string
	Headers       map[string]string
	Logger        *logrus.Logger
	Version       string
	Debug         bool
	rClient       *resty.Client
	Proxy         string
	LogTo         string
	LogFile       *os.File
	Platform      string
	GoArch        string
	ServiceConfig *service.Config
}

const (
	progFilesName = "TacticalAgent"
	winExeName    = "tacticalrmm.exe"
	winSvcName    = "tacticalrmm"
	meshSvcName   = "mesh agent"
)

var natsCheckin = []string{"agent-hello", "agent-agentinfo", "agent-disks", "agent-winsvc", "agent-publicip", "agent-wmi"}

func New(logger *logrus.Logger, version string) *Agent {
	host, _ := ps.Host()
	info := host.Info()
	pd := filepath.Join(os.Getenv("ProgramFiles"), progFilesName)
	exe := filepath.Join(pd, winExeName)
	sd := os.Getenv("SystemDrive")

	var pybin string
	switch runtime.GOARCH {
	case "amd64":
		pybin = filepath.Join(pd, "py38-x64", "python.exe")
	case "386":
		pybin = filepath.Join(pd, "py38-x32", "python.exe")
	}

	ac := NewAgentConfig()

	headers := make(map[string]string)
	if len(ac.Token) > 0 {
		headers["Content-Type"] = "application/json"
		headers["Authorization"] = fmt.Sprintf("Token %s", ac.Token)
	}

	restyC := resty.New()
	restyC.SetBaseURL(ac.BaseURL)
	restyC.SetCloseConnection(true)
	restyC.SetHeaders(headers)
	restyC.SetTimeout(15 * time.Second)
	restyC.SetDebug(logger.IsLevelEnabled(logrus.DebugLevel))

	if len(ac.Proxy) > 0 {
		restyC.SetProxy(ac.Proxy)
	}
	if len(ac.Cert) > 0 {
		restyC.SetRootCertificate(ac.Cert)
	}

	var MeshSysExe string
	if len(ac.CustomMeshDir) > 0 {
		MeshSysExe = filepath.Join(ac.CustomMeshDir, "MeshAgent.exe")
	} else {
		MeshSysExe = filepath.Join(os.Getenv("ProgramFiles"), "Mesh Agent", "MeshAgent.exe")
	}

	if runtime.GOOS == "linux" {
		MeshSysExe = "/opt/tacticalmesh/meshagent"
	}

	svcConf := &service.Config{
		Executable:  exe,
		Name:        winSvcName,
		DisplayName: "TacticalRMM Agent Service",
		Arguments:   []string{"-m", "svc"},
		Description: "TacticalRMM Agent Service",
		Option: service.KeyValue{
			"StartType":              "automatic",
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "5s",
			"OnFailureResetPeriod":   10,
		},
	}

	return &Agent{
		Hostname:      info.Hostname,
		Arch:          info.Architecture,
		BaseURL:       ac.BaseURL,
		AgentID:       ac.AgentID,
		ApiURL:        ac.APIURL,
		Token:         ac.Token,
		AgentPK:       ac.PK,
		Cert:          ac.Cert,
		ProgramDir:    pd,
		EXE:           exe,
		SystemDrive:   sd,
		MeshInstaller: "meshagent.exe",
		MeshSystemEXE: MeshSysExe,
		MeshSVC:       meshSvcName,
		PyBin:         pybin,
		Headers:       headers,
		Logger:        logger,
		Version:       version,
		Debug:         logger.IsLevelEnabled(logrus.DebugLevel),
		rClient:       restyC,
		Proxy:         ac.Proxy,
		Platform:      runtime.GOOS,
		GoArch:        runtime.GOARCH,
		ServiceConfig: svcConf,
	}
}

type CmdStatus struct {
	Status gocmd.Status
	Stdout string
	Stderr string
}

type CmdOptions struct {
	Shell        string
	Command      string
	Args         []string
	Timeout      time.Duration
	IsScript     bool
	IsExecutable bool
	Detached     bool
}

func (a *Agent) NewCMDOpts() *CmdOptions {
	return &CmdOptions{
		Shell:   "/bin/bash",
		Timeout: 30,
	}
}

func (a *Agent) CmdV2(c *CmdOptions) CmdStatus {

	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout*time.Second)
	defer cancel()

	// Disable output buffering, enable streaming
	cmdOptions := gocmd.Options{
		Buffered:  false,
		Streaming: true,
	}

	// have a child process that is in a different process group so that
	// parent terminating doesn't kill child
	if c.Detached {
		cmdOptions.BeforeExec = []func(cmd *exec.Cmd){
			func(cmd *exec.Cmd) {
				cmd.SysProcAttr = SetDetached()
			},
		}
	}

	var envCmd *gocmd.Cmd
	if c.IsScript {
		envCmd = gocmd.NewCmdOptions(cmdOptions, c.Shell, c.Args...) // call script directly
	} else if c.IsExecutable {
		envCmd = gocmd.NewCmdOptions(cmdOptions, c.Shell, c.Command) // c.Shell: bin + c.Command: args as one string
	} else {
		envCmd = gocmd.NewCmdOptions(cmdOptions, c.Shell, "-c", c.Command) // /bin/bash -c 'ls -l /var/log/...'
	}

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	// Print STDOUT and STDERR lines streaming from Cmd
	doneChan := make(chan struct{})
	go func() {
		defer close(doneChan)
		// Done when both channels have been closed
		// https://dave.cheney.net/2013/04/30/curious-channels
		for envCmd.Stdout != nil || envCmd.Stderr != nil {
			select {
			case line, open := <-envCmd.Stdout:
				if !open {
					envCmd.Stdout = nil
					continue
				}
				fmt.Fprintln(&stdoutBuf, line)
				a.Logger.Debugln(line)

			case line, open := <-envCmd.Stderr:
				if !open {
					envCmd.Stderr = nil
					continue
				}
				fmt.Fprintln(&stderrBuf, line)
				a.Logger.Debugln(line)
			}
		}
	}()

	// Run and wait for Cmd to return, discard Status
	envCmd.Start()

	go func() {
		select {
		case <-doneChan:
			return
		case <-ctx.Done():
			a.Logger.Debugf("Command timed out after %d seconds\n", c.Timeout)
			pid := envCmd.Status().PID
			a.Logger.Debugln("Killing process with PID", pid)
			KillProc(int32(pid))
		}
	}()

	// Wait for goroutine to print everything
	<-doneChan
	ret := CmdStatus{
		Status: envCmd.Status(),
		Stdout: CleanString(stdoutBuf.String()),
		Stderr: CleanString(stderrBuf.String()),
	}
	a.Logger.Debugf("%+v\n", ret)
	return ret
}

func (a *Agent) GetCPULoadAvg() int {
	fallback := false
	pyCode := `
import psutil
try:
	print(int(round(psutil.cpu_percent(interval=10))), end='')
except:
	print("pyerror", end='')
`
	pypercent, err := a.RunPythonCode(pyCode, 13, []string{})
	if err != nil || pypercent == "pyerror" {
		fallback = true
	}

	i, err := strconv.Atoi(pypercent)
	if err != nil {
		fallback = true
	}

	if fallback {
		percent, err := cpu.Percent(10*time.Second, false)
		if err != nil {
			a.Logger.Debugln("Go CPU Check:", err)
			return 0
		}
		return int(math.Round(percent[0]))
	}
	return i
}

// ForceKillMesh kills all mesh agent related processes
func (a *Agent) ForceKillMesh() {
	pids := make([]int, 0)

	procs, err := ps.Processes()
	if err != nil {
		return
	}

	for _, process := range procs {
		p, err := process.Info()
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(p.Name), "meshagent") {
			pids = append(pids, p.PID)
		}
	}

	for _, pid := range pids {
		a.Logger.Debugln("Killing mesh process with pid %d", pid)
		if err := KillProc(int32(pid)); err != nil {
			a.Logger.Debugln(err)
		}
	}
}

func (a *Agent) SyncMeshNodeID() {

	id, err := a.getMeshNodeID()
	if err != nil {
		a.Logger.Errorln("SyncMeshNodeID() getMeshNodeID()", err)
		return
	}

	payload := rmm.MeshNodeID{
		Func:    "syncmesh",
		Agentid: a.AgentID,
		NodeID:  StripAll(id),
	}

	_, err = a.rClient.R().SetBody(payload).Post("/api/v3/syncmesh/")
	if err != nil {
		a.Logger.Debugln("SyncMesh:", err)
	}
}

func (a *Agent) setupNatsOptions() []nats.Option {
	opts := make([]nats.Option, 0)
	opts = append(opts, nats.Name("TacticalRMM"))
	opts = append(opts, nats.UserInfo(a.AgentID, a.Token))
	opts = append(opts, nats.ReconnectWait(time.Second*5))
	opts = append(opts, nats.RetryOnFailedConnect(true))
	opts = append(opts, nats.MaxReconnects(-1))
	opts = append(opts, nats.ReconnectBufSize(-1))
	return opts
}

func (a *Agent) GetUninstallExe() string {
	cderr := os.Chdir(a.ProgramDir)
	if cderr == nil {
		files, err := filepath.Glob("unins*.exe")
		if err == nil {
			for _, f := range files {
				if strings.Contains(f, "001") {
					return f
				}
			}
		}
	}
	return "unins000.exe"
}

func (a *Agent) CleanupAgentUpdates() {
	cderr := os.Chdir(a.ProgramDir)
	if cderr != nil {
		a.Logger.Errorln(cderr)
		return
	}

	files, err := filepath.Glob("winagent-v*.exe")
	if err == nil {
		for _, f := range files {
			os.Remove(f)
		}
	}

	cderr = os.Chdir(os.Getenv("TMP"))
	if cderr != nil {
		a.Logger.Errorln(cderr)
		return
	}
	folders, err := filepath.Glob("tacticalrmm*")
	if err == nil {
		for _, f := range folders {
			os.RemoveAll(f)
		}
	}
}

func (a *Agent) RunPythonCode(code string, timeout int, args []string) (string, error) {
	content := []byte(code)
	dir, err := ioutil.TempDir("", "tacticalpy")
	if err != nil {
		a.Logger.Debugln(err)
		return "", err
	}
	defer os.RemoveAll(dir)

	tmpfn, _ := ioutil.TempFile(dir, "*.py")
	if _, err := tmpfn.Write(content); err != nil {
		a.Logger.Debugln(err)
		return "", err
	}
	if err := tmpfn.Close(); err != nil {
		a.Logger.Debugln(err)
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	var outb, errb bytes.Buffer
	cmdArgs := []string{tmpfn.Name()}
	if len(args) > 0 {
		cmdArgs = append(cmdArgs, args...)
	}
	a.Logger.Debugln(cmdArgs)
	cmd := exec.CommandContext(ctx, a.PyBin, cmdArgs...)
	cmd.Stdout = &outb
	cmd.Stderr = &errb

	cmdErr := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		a.Logger.Debugln("RunPythonCode:", ctx.Err())
		return "", ctx.Err()
	}

	if cmdErr != nil {
		a.Logger.Debugln("RunPythonCode:", cmdErr)
		return "", cmdErr
	}

	if errb.String() != "" {
		a.Logger.Debugln(errb.String())
		return errb.String(), errors.New("RunPythonCode stderr")
	}

	return outb.String(), nil

}

func (a *Agent) CreateTRMMTempDir() {
	// create the temp dir for running scripts
	dir := filepath.Join(os.TempDir(), "trmm")
	if !trmm.FileExists(dir) {
		err := os.Mkdir(dir, 0775)
		if err != nil {
			a.Logger.Errorln(err)
		}
	}
}
