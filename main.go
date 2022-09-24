package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/neo-mode/runner-api"
)

type Config struct {
	Token             string
	ConnectionTimeout time.Duration

	Shell   string
	WorkDir string

	Protection   bool
	CacheSucceed bool

	Jobs []ConfigJob
}

type ConfigJob struct {
	ProjectID string
	JobName   string

	Cmd   string
	Args  []string
	Stdin []string
}

type Job struct {
	ID        json.Number
	Token     string
	JobInfo   JobInfo `json:"job_info"`
	GitInfo   GitInfo `json:"git_info"`
	Variables []Variable
	Steps     []Step
}

type JobInfo struct {
	Stage     string
	Name      string
	ProjectID json.Number `json:"project_id"`
}

type GitInfo struct {
	RepoURL string `json:"repo_url"`
	Sha     string
}

type Variable struct {
	Key    string
	Value  string
	Public bool
}

type Step struct {
	Name   string
	Script []string
}

type State struct {
	Token    string `json:"token"`
	State    string `json:"state,omitempty"`
	Failure  string `json:"failure_reason,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

var config Config

var projID string
var projDir string
var pipelineID string
var target string
var isMergeDone bool

var job *Job
var trace *bytes.Buffer

func main() {

	var homeDir = os.Getenv("HOME")
	if homeDir == "" {
		return
	}

	defineConfig(homeDir)
	var err error
	if err = os.MkdirAll(config.WorkDir, 0755); err != nil {
		printErr(err.Error())
	}

	var found bool
	var jobID string
	var state State

	job = new(Job)
	trace = new(bytes.Buffer)

	for {
		found, err = runner.Request(url.Values{"info[features][refspecs]": []string{"true"}, "info[features][return_exit_code]": []string{"true"}, "token": []string{config.Token}}, job)
		if err != nil {
			printErr(err.Error())
		}
		if !found {
			break
		}

		jobID = string(job.ID)
		projID = string(job.JobInfo.ProjectID)
		projDir = config.WorkDir + "/" + projID

		state.Token = job.Token
		state.State = "success"
		state.ExitCode = 0
		state.Failure = ""

		if err = handleJob(); err != nil {
			state.State = "failed"

			switch err := err.(type) {
			case *exec.ExitError:
				state.ExitCode = err.ExitCode()
				state.Failure = "script_failure"

			case runner.APIError:
				state.Failure = "api_failure"

			default:
				state.Failure = "runner_system_failure"
			}
		}

		runner.SendTrace(jobID, job.Token, trace)
		runner.Update(jobID, state)

		trace.Reset()
		time.Sleep(time.Second)
	}
}

func handleJob() error {

	var configJob *ConfigJob
	var jobName = job.JobInfo.Name

	for _, val := range config.Jobs {
		if (val.ProjectID == "" || val.ProjectID == projID) && val.JobName == jobName {
			configJob = &val
			break
		}
	}

	if config.Protection && configJob == nil {
		return runner.APIError("")
	}

	var targetName, sourceName, mergeID, _pipelineID string
	for _, val := range job.Variables {

		if val.Public {
			os.Setenv(val.Key, val.Value)
		}

		if val.Key == "CI_MERGE_REQUEST_TARGET_BRANCH_NAME" {
			targetName = val.Value

		} else if val.Key == "CI_MERGE_REQUEST_SOURCE_BRANCH_NAME" {
			sourceName = val.Value

		} else if val.Key == "CI_MERGE_REQUEST_IID" {
			mergeID = val.Value

		} else if val.Key == "CI_PIPELINE_IID" {
			_pipelineID = val.Value
		}
	}

	var err error
	var isMerge = targetName != "" && sourceName != ""
	var isNewPipeline = pipelineID != _pipelineID
	var refDir = "refs/merged/" + targetName

	if isNewPipeline {

		var info = job.GitInfo
		var isTargetUpdated bool
		if isTargetUpdated, err = runner.UpdateRefs(projDir, targetName, sourceName, info.Sha, info.RepoURL); err != nil {
			return err
		}

		var source string
		if isMerge {
			if isTargetUpdated {
				os.RemoveAll(projDir + "/.git/" + refDir)
			} else {
				target = runner.GetRef(projDir, refDir+"/"+mergeID)
			}
			if target == "" {
				target = "origin/" + targetName
			}
			source = "origin/" + sourceName
		} else {
			target = info.Sha
		}

		if isMergeDone, err = runner.Checkout(projDir, target, source); err != nil {
			return err
		}

		pipelineID = _pipelineID
	}

	if isMerge && config.CacheSucceed {
		if isMergeDone {
			if target == runner.GetRef(projDir, refDir+"/"+mergeID+"-"+jobName) {
				return nil
			}
		} else {
			runner.SetRef(projDir, refDir, mergeID, "HEAD")
		}
	}

	if configJob != nil {

		if err = execScript(configJob.Cmd, configJob.Args, configJob.Stdin); err != nil {
			return err
		}

		if isMerge && config.CacheSucceed {
			runner.SetRef(projDir, refDir, mergeID+"-"+jobName, "HEAD")
		}

		return nil
	}

	var before, script, after []string
	for _, val := range job.Steps {

		if val.Name == "before_script" {
			before = val.Script

		} else if val.Name == "script" {
			script = val.Script

		} else if val.Name == "after_script" {
			after = val.Script
		}
	}

	if before != nil {
		if err = execScript(config.Shell, nil, before); err != nil {
			return err
		}
	}

	err = execScript(config.Shell, nil, script)

	if after != nil {
		execScript(config.Shell, nil, after)
	}

	if err != nil {
		return err
	}

	if isMerge && config.CacheSucceed {
		runner.SetRef(projDir, refDir, mergeID+"-"+jobName, "HEAD")
	}

	return nil
}

func execScript(name string, args []string, stdin []string) error {

	var cmd = exec.Command(name, args...)
	cmd.Dir = projDir
	cmd.Stdout = trace
	cmd.Stderr = trace

	if stdin == nil {
		return cmd.Run()
	}

	var data bytes.Buffer
	for _, val := range stdin {
		data.WriteString(val + "\n")
	}

	cmd.Stdin = &data
	return cmd.Run()
}

func defineConfig(homeDir string) {

	var confName = homeDir + "/.ci-config.json"
	var f, err = os.Open(confName)
	if err == nil {

		err = json.NewDecoder(f).Decode(&config)
		f.Close()

		if err != nil {
			printErr(err.Error())
		}

		runner.Client = &http.Client{Timeout: time.Second * config.ConnectionTimeout}
		return
	}

	if !os.IsNotExist(err) {
		printErr(err.Error())
	}

	var token string
	println("Input GitLab token")

	var n, _ = fmt.Scanln(&token)
	if n <= 0 {
		printErr("Cancelled")
	}

	runner.Client = &http.Client{Timeout: time.Second * 10}
	token, err = runner.Register(url.Values{"token": []string{token}})
	if err != nil {
		printErr(err.Error())
	}

	config.Token = token
	config.ConnectionTimeout = 10
	config.WorkDir = homeDir + "/.ci"
	config.Shell = "sh"
	config.Jobs = []ConfigJob{{JobName: "test-job"}}

	f, err = os.OpenFile(confName, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		printErr(err.Error() + ". Registered token: " + token)
	}

	var enc = json.NewEncoder(f)
	enc.SetIndent("", "\t")
	enc.Encode(&config)
	f.Close()

	println("Runner has been registered successfully. Config path is: " + confName)
	os.Exit(0)
}

func printErr(text string) {
	os.Stderr.WriteString(text + "\n")
	os.Exit(1)
}
