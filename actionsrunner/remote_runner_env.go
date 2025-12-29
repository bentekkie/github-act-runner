package actionsrunner

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ChristopherHX/github-act-runner/protocol"
	rc "github.com/bazelbuild/remote-apis-sdks/go/pkg/client"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/command"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/filemetadata"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/outerr"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/rexec"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/uploadinfo"
)

//go:embed stdinrun
var stdinrunner []byte

var runScript = `#!/bin/bash

set -xe
mkfifo stdinpipe
exec 3<> stdinpipe 
{
	cat input.bin >stdinpipe &
} &

./runner worker < stdinpipe
exec 3>&-
`

type RemoteEnv struct {
	files map[string][]byte

	Client *rexec.Client
	Image  string
}

func NewRemoteEnv(ctx context.Context) (*RemoteEnv, error) {
	grpcClient, err := rc.NewClient(ctx, os.Getenv("RUNNER_RBE_INSTANCE"), rc.DialParams{
		Service:               os.Getenv("RUNNER_RBE_SERVICE"),
		UseApplicationDefault: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %s", err)
	}
	return &RemoteEnv{
		files:  make(map[string][]byte),
		Client: &rexec.Client{GrpcClient: grpcClient, FileMetadataCache: filemetadata.NewSingleFlightCache()},
		Image:  os.Getenv("RUNNER_RBE_IMAGE"),
	}, nil
}

func (arunner *RemoteEnv) WriteJSON(path string, value interface{}) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	arunner.files[path] = b
	return nil
}

func (arunner *RemoteEnv) ReadJSON(path string, value interface{}) error {
	cont, ok := arunner.files[path]
	if !ok {
		return os.ErrNotExist
	}
	return json.Unmarshal(cont, value)
}

func (arunner *RemoteEnv) Remove(fname string) error {
	delete(arunner.files, fname)
	return nil
}

func (arunner *RemoteEnv) Printf(format string, a ...interface{}) {
	fmt.Printf(format, a...)
}

func (arunner *RemoteEnv) ExecWorker(
	_ *RunRunner, wc WorkerContext, _ *protocol.AgentJobRequestMessage, src []byte,
) error {
	var vis []*command.VirtualInput
	var ues []*uploadinfo.Entry
	for path, content := range arunner.files {
		dg := digest.NewFromBlob(content)
		vis = append(vis, &command.VirtualInput{
			Path:   path,
			Digest: dg.String(),
		})
		ues = append(ues, uploadinfo.EntryFromBlob(content))
	}

	runnerExec, err := os.Executable()
	if err != nil {
		fmt.Printf("failed to get executable: %s", err)
		return err
	}
	runnerDg, runnerDgErr := digest.NewFromFile(runnerExec)
	if runnerDgErr != nil {
		fmt.Printf("failed to get digest: %s", runnerDgErr)
		return runnerDgErr
	}
	vis = append(vis, &command.VirtualInput{
		Path:         "runner",
		Digest:       runnerDg.String(),
		IsExecutable: true,
	})
	ues = append(ues, uploadinfo.EntryFromFile(runnerDg, runnerExec))

	iue := uploadinfo.EntryFromBlob(src)
	vis = append(vis, &command.VirtualInput{
		Path:   "input.json",
		Digest: iue.Digest.String(),
	})
	ues = append(ues, iue)

	mid := make([]byte, messageIDSize)
	var in bytes.Buffer
	binary.BigEndian.PutUint32(mid, 1) // NewJobRequest
	_, err = in.Write(mid)
	if err != nil {
		fmt.Printf("failed to write new job: %s", err)
	}
	binary.BigEndian.PutUint32(mid, uint32(len(src))) //nolint:gosec
	_, err = in.Write(mid)
	if err != nil {
		fmt.Printf("failed to write new job: %s", err)
	}
	_, err = in.Write(src)
	if err != nil {
		fmt.Printf("failed to write new job: %s", err)
	}

	iiue := uploadinfo.EntryFromBlob(in.Bytes())
	vis = append(vis, &command.VirtualInput{
		Path:   "input.bin",
		Digest: iiue.Digest.String(),
	})
	ues = append(ues, iiue)

	runnerScriptDg := digest.NewFromBlob([]byte(runScript))
	vis = append(vis, &command.VirtualInput{
		Path:         "run.sh",
		Digest:       runnerScriptDg.String(),
		IsExecutable: true,
	})
	ues = append(ues, uploadinfo.EntryFromBlob([]byte(runScript)))

	_, _, err = arunner.Client.GrpcClient.UploadIfMissing(wc.JobExecCtx(), ues...)
	if err != nil {
		fmt.Printf("failed to upload inputs: %s", err)
		return err
	}

	tmpDir, err := os.MkdirTemp("", "runner-exec-root-")
	if err != nil {
		fmt.Printf("failed to create temp dir: %s", err)
		return err
	}
	jlogger := wc.Logger()
	_ = jlogger.Logger.Close() // Ignore logger close errors
	jlogger.Current().Complete("Succeeded")
	jlogger.MoveNext()
	resp, _ := arunner.Client.Run(wc.JobExecCtx(), &command.Command{
		Args: []string{
			"./run.sh",
		},
		ExecRoot: tmpDir,
		InputSpec: &command.InputSpec{
			VirtualInputs: vis,
		},
		OutputDirs: []string{
			"_work",
		},
		Platform: map[string]string{
			"container-image": "docker://" + arunner.Image,
			"dockerNetwork":   "standard",
			"dockerRuntime":   "runc",
		},
	},
		&command.ExecutionOptions{
			AcceptCached: false,
			StreamOutErr: true,
		}, outerr.NewStreamOutErr(os.Stdout, os.Stderr))

	if resp.ExitCode != 0 {
		fmt.Printf("remote runner failed with exit code %d\n%v\n", resp.ExitCode, resp.Err)
		return fmt.Errorf("remote runner failed with exit code %d: %w", resp.ExitCode, resp.Err)
	}
	return nil
}
