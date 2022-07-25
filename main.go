package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"time"
)

type Config struct {
	Token string

	ConnectionTimeout  time.Duration
	TimeoutBetweenJobs time.Duration

	Shell   string
	WorkDir string

	DoMerge    bool
	Protection bool
	Env        map[string]string

	Jobs []ConfigJob
}

type ConfigJob struct {
	ProjectID    string
	JobName      string
	BeforeScript []string
	Script       []string
	AfterScript  []string
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
	RepoURL  string `json:"repo_url"`
	Sha      string
	Refspecs []string
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

type APIError string

const refsRemotes = "refs/remotes/"
const origin = "origin/"

var config Config
var client http.Client

var jobID string
var jobToken string

var projID string
var projDir string

func main() {

	var homeDir = os.Getenv("HOME")
	if homeDir == "" {
		printErr("HOME env not found. Cancelled")
	}

	var f, err = os.Open(homeDir + "/.ci-config.json")
	if err != nil {
		if !os.IsNotExist(err) {
			printErr(err.Error())
		}
		registerRunner(homeDir)
		return
	}

	err = json.NewDecoder(f).Decode(&config)
	f.Close()

	if err != nil {
		printErr(err.Error())
	}

	client.Timeout = time.Second * config.ConnectionTimeout

	var job *Job

	f, _ = os.Open(config.WorkDir + "/job.json")
	if f != nil {
		job = new(Job)
		err = json.NewDecoder(f).Decode(job)
		f.Close()
		if err != nil {
			printErr(err.Error())
		}
	}

	for key, val := range config.Env {
		os.Setenv(key, val)
	}

	var state State
	var trace bytes.Buffer

	for {
		job = requestJob(job, config.Token)
		if job == nil {
			break
		}

		jobID = string(job.ID)
		jobToken = job.Token

		projID = string(job.JobInfo.ProjectID)
		projDir = config.WorkDir + "/" + projID

		state.Token = jobToken
		state.State = "success"
		state.ExitCode = 0

		err = handleJob(job, &trace)
		job = nil

		if err != nil {
			state.State = "failed"
			switch err := err.(type) {
			case *exec.ExitError:
				state.ExitCode = err.ExitCode()
				state.Failure = "script_failure"

			case APIError:
				state.ExitCode = 127
				state.Failure = "api_failure"

			default:
				state.ExitCode = 128
				state.Failure = "runner_system_failure"
			}
		}

		sendTrace(&trace)
		updateJob(&state)

		trace.Reset()

		if config.TimeoutBetweenJobs > 0 {
			time.Sleep(time.Second * config.TimeoutBetweenJobs)
		}
	}

	os.Remove(config.WorkDir + "/job.json")
}

func (api APIError) Error() string {
	return string(api)
}

func registerRunner(homeDir string) {

	println("Input the GitLab token and press [ENTER]")

	var token string
	var n, _ = fmt.Scanln(&token)
	if n <= 0 {
		printErr("Cancelled")
	}

	var configPath = homeDir + "/.ci-config.json"
	var f, err = os.OpenFile(configPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		printErr(err.Error())
	}
	defer f.Close()

	config.ConnectionTimeout = 5
	config.TimeoutBetweenJobs = 1

	config.Shell = "sh"
	config.WorkDir = homeDir + "/.ci"

	config.DoMerge = true
	config.Jobs = []ConfigJob{{JobName: "test-job", BeforeScript: []string{}, Script: []string{}, AfterScript: []string{}}}

	client.Timeout = time.Second * 5

	var res *http.Response
	res, err = client.PostForm("https://gitlab.com/api/v4/runners", url.Values{"token": []string{token}})
	if err != nil {
		printErr(err.Error())
	}

	if res.StatusCode != 201 {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		printErr(res.Status)
	}

	var register struct {
		Token string
	}

	err = json.NewDecoder(res.Body).Decode(&register)
	res.Body.Close()

	if err != nil {
		printErr(err.Error())
	}

	config.Token = register.Token

	var enc = json.NewEncoder(f)
	enc.SetIndent("", "\t")
	enc.Encode(config)

	println("\nRunner has been registered successfully. Config file is located in: " + configPath)
}

func requestJob(job *Job, token string) *Job {

	if job != nil {
		return job
	}

	var res, err = client.PostForm("https://gitlab.com/api/v4/jobs/request", url.Values{"info[features][refspecs]": []string{"true"}, "info[features][return_exit_code]": []string{"true"}, "token": []string{token}})
	if err != nil {
		printErr(err.Error())
	}

	var is204 = res.StatusCode == 204
	if is204 || res.StatusCode != 201 {

		io.Copy(io.Discard, res.Body)
		res.Body.Close()

		if is204 {
			return nil
		}

		printErr(res.Status)
	}

	job = new(Job)
	err = json.NewDecoder(res.Body).Decode(job)
	res.Body.Close()

	if err != nil {
		printErr(err.Error())
	}

	var f, _ = os.OpenFile(config.WorkDir+"/job.json", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if f != nil {
		json.NewEncoder(f).Encode(job)
		f.Close()
	}

	return job
}

func updateJob(state *State) {

	var enc, err = json.Marshal(state)
	if err != nil {
		printErr(err.Error())
	}

	var req *http.Request
	req, err = http.NewRequest(http.MethodPut, "https://gitlab.com/api/v4/jobs/"+jobID, bytes.NewReader(enc))
	if err != nil {
		printErr(err.Error())
	}

	req.Header.Set("Content-Type", "application/json")

	var res *http.Response
	res, err = client.Do(req)
	if err != nil {
		printErr(err.Error())
	}

	io.Copy(io.Discard, req.Body)
	res.Body.Close()
}

func sendTrace(trace io.Reader) bool {

	var req, err = http.NewRequest(http.MethodPatch, "https://gitlab.com/api/v4/jobs/"+jobID+"/trace", trace)
	if err != nil {
		printErr(err.Error())
	}

	if req.ContentLength <= 0 {
		return false
	}

	req.Header.Set("JOB-TOKEN", jobToken)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Content-Range", "0-"+strconv.FormatInt(req.ContentLength, 10))

	var res *http.Response
	res, err = client.Do(req)
	if err != nil {
		printErr(err.Error())
	}

	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	return true
}

func getConfigJob(job *Job) ([]string, []string, []string, bool) {

	for _, val := range config.Jobs {
		if (val.ProjectID == "" || val.ProjectID == projID) && val.JobName == job.JobInfo.Name {
			return val.BeforeScript, val.Script, val.AfterScript, true
		}
	}

	return nil, nil, nil, false
}

func handleJob(job *Job, trace io.Writer) error {

	var before, script, after, found = getConfigJob(job)
	if !found && config.Protection {
		return APIError("")
	}

	var err error
	if err = os.MkdirAll(config.WorkDir, 0755); err != nil {
		return err
	}

	if err = os.Mkdir(projDir, 0755); err != nil && !os.IsExist(err) {
		return err
	}

	var targetName, sourceName, oldTarget, mergeID string
	var isMerge = config.DoMerge

	for _, val := range job.Variables {

		if val.Public {
			os.Setenv(val.Key, val.Value)
		}

		if isMerge {
			if val.Key == "CI_MERGE_REQUEST_TARGET_BRANCH_NAME" {
				targetName = val.Value

			} else if val.Key == "CI_MERGE_REQUEST_SOURCE_BRANCH_NAME" {
				sourceName = val.Value

			} else if val.Key == "CI_MERGE_REQUEST_IID" {
				mergeID = val.Value
			}
		}
	}

	if isMerge && targetName == "" {
		isMerge = false
	}

	if err == nil {

		if err = git("clone", job.GitInfo.RepoURL, "."); err != nil {
			return err
		}

	} else {

		var args []string
		if isMerge {
			oldTarget = getTarget(targetName)
			args = []string{"fetch", job.GitInfo.RepoURL, "+" + targetName + ":" + refsRemotes + origin + targetName, "+" + sourceName + ":" + refsRemotes + origin + sourceName}
		} else {
			args = make([]string, len(job.GitInfo.Refspecs)+2)
			args[0] = "fetch"
			args[1] = job.GitInfo.RepoURL
			for key, val := range job.GitInfo.Refspecs {
				args[key+2] = val
			}
		}

		if err = git(args...); err != nil {
			return err
		}
	}

	if isMerge {

		var target = getTarget(targetName)
		if target == oldTarget {
			target = getMergedTarget(targetName, mergeID)
		} else {
			os.RemoveAll(projDir + "/.git/refs/merged/" + targetName)
		}

		if err = git("checkout", target); err != nil {
			return err
		}

		var cmd = exec.Command("git", "merge", origin+sourceName)
		cmd.Dir = projDir
		var data, err = cmd.Output()

		if err != nil {
			git("reset", "--merge")
			return APIError("")
		}

		var _data = string(data)
		if _data == "Already up to date.\n" || _data == "Merge made by the 'recursive' strategy.\n" {
			return nil
		}

	} else if err = git("checkout", job.GitInfo.Sha); err != nil {
		return err
	}

	if !found {
		for _, val := range job.Steps {

			if val.Name == "before_script" {
				before = val.Script

			} else if val.Name == "script" {
				script = val.Script

			} else if val.Name == "after_script" {
				after = val.Script
			}
		}
	}

	if before != nil {
		if err = handleScript(before, trace); err != nil {
			return err
		}
	}

	if script != nil {
		err = handleScript(script, trace)
	}

	if after != nil {
		handleScript(after, trace)
	}

	if isMerge && err == nil {
		setMergedTarget(targetName, mergeID)
	}

	return err
}

func handleScript(script []string, trace io.Writer) error {

	var data bytes.Buffer
	for _, val := range script {
		data.WriteString(val + "\n")
	}

	var cmd = exec.Command(config.Shell)
	cmd.Stdin = &data
	cmd.Stdout = trace
	cmd.Stderr = trace
	cmd.Dir = projDir
	return cmd.Run()
}

func getTarget(target string) string {

	var f, _ = os.Open(projDir + "/.git/" + refsRemotes + origin + target)
	if f == nil {
		return ""
	}

	var data = make([]byte, 32)
	f.Read(data)
	f.Close()

	return string(data)
}

func getMergedTarget(targetName, source string) string {

	var f, _ = os.Open(projDir + "/.git/refs/merged/" + targetName + "/" + source)
	if f == nil {
		return origin + targetName
	}

	var data = make([]byte, 32)
	f.Read(data)
	f.Close()

	return string(data)
}

func setMergedTarget(targetName, source string) error {

	var src, err = os.Open(projDir + "/.git/HEAD")
	if src == nil {
		return err
	}

	targetName = projDir + "/.git/refs/merged/" + targetName
	if err = os.MkdirAll(targetName, 0755); err != nil {
		src.Close()
		return err
	}

	var dst *os.File
	dst, err = os.OpenFile(targetName+"/"+source, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		src.Close()
		return err
	}

	io.Copy(dst, src)
	src.Close()
	dst.Close()

	return nil
}

func git(args ...string) error {
	var cmd = exec.Command("git", args...)
	cmd.Dir = projDir
	return cmd.Run()
}

func printErr(text string) {
	os.Stderr.WriteString(text + "\n")
	os.Exit(1)
}
