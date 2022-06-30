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
	"strings"
	"time"
)

type Config struct {
	Token      string            `json:"token"`
	Timeout    time.Duration     `json:"connection_timeout"`
	Shell      string            `json:"shell"`
	WorkDir    string            `json:"work_dir"`
	JobTimeout time.Duration     `json:"timeout_between_jobs"`
	Merge      bool              `json:"merge"`
	Env        map[string]string `json:"env"`
}

type Register struct {
	Token string
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
	RepoURL   string `json:"repo_url"`
	Sha       string
	BeforeSha string `json:"before_sha"`
	Refspecs  []string
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
var client http.Client

var jobID string
var jobToken string
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

	client.Timeout = time.Second * config.Timeout

	var job *Job
	var state *State
	var trace *bytes.Buffer

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

	for {
		job = requestJob(job, config.Token)
		if job == nil {
			break
		}

		jobID = string(job.ID)
		jobToken = job.Token
		projDir = config.WorkDir + "/" + string(job.JobInfo.ProjectID)

		state = &State{Token: jobToken, State: "success", ExitCode: 0}
		trace = new(bytes.Buffer)
		err = handleJob(job, trace)
		job = nil

		if err != nil {
			state.State = "failed"
			if err, ok := err.(*exec.ExitError); ok {
				state.ExitCode = err.ExitCode()
				state.Failure = "script_failure"
			} else {
				state.ExitCode = 1
				state.Failure = "runner_system_failure"
			}
		}

		sendTrace(trace)
		updateJob(state)

		if config.JobTimeout > 0 {
			time.Sleep(time.Second * config.JobTimeout)
		}
	}

	os.Remove(config.WorkDir + "/job.json")
}

func registerRunner(homeDir string) {

	var token = ""
	println("Input the GitLab token and press [ENTER]")
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

	config.Timeout = 5
	config.Shell = "sh"
	config.WorkDir = homeDir + "/.ci"
	config.JobTimeout = 1
	config.Merge = true
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

	var register = new(Register)
	err = json.NewDecoder(res.Body).Decode(register)
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

	if res.StatusCode != 200 {
		os.Remove(config.WorkDir + "/job.json")
		printErr(res.Status)
	}
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

func handleJob(job *Job, trace io.Writer) error {

	os.MkdirAll(config.WorkDir, 0755)

	var err error
	if err = os.Mkdir(projDir, 0755); err != nil && !os.IsExist(err) {
		return err
	}

	var targetName, oldTarget, sourceID string
	var isMerge = config.Merge && job.GitInfo.BeforeSha == "0000000000000000000000000000000000000000"

	for _, val := range job.Variables {
		if val.Public {
			os.Setenv(val.Key, val.Value)
		}
		if isMerge {
			if val.Key == "CI_MERGE_REQUEST_TARGET_BRANCH_NAME" {
				targetName = val.Value
			} else if val.Key == "CI_MERGE_REQUEST_IID" {
				sourceID = val.Value
			}
		}
	}

	if err == nil {

		if err = git("clone", job.GitInfo.RepoURL, "."); err != nil {
			return err
		}

	} else {

		var offset = 2
		if isMerge {
			offset++
			oldTarget = getRefSHA(targetName)
		}

		var args = make([]string, len(job.GitInfo.Refspecs)+offset)
		args[0] = "fetch"
		args[1] = job.GitInfo.RepoURL

		if isMerge {
			args[2] = targetName
		}

		for key, val := range job.GitInfo.Refspecs {
			args[key+offset] = val
		}

		if err = git(args...); err != nil {
			return err
		}
	}

	if isMerge {

		var target = getRefSHA(targetName)
		if target == oldTarget {

			if err = git("checkout", targetName+"/"+sourceID); err != nil {
				if err = git("checkout", target); err != nil {
					return err
				}
			}

		} else {

			if oldTarget != "" {
				os.RemoveAll(projDir + "/.git/refs/" + targetName)
			}

			if targetName != target {
				if err = git("checkout", target); err != nil {
					return err
				}
			}
		}

		var cmd = exec.Command("git", "merge", job.GitInfo.Sha)
		cmd.Dir = projDir
		var data, err = cmd.Output()

		if err != nil {
			git("reset", "--merge")
			return err
		}

		if string(data[:19]) == "Already up to date." {
			return nil
		}

	} else {

		if err = git("checkout", job.GitInfo.Sha); err != nil {
			return err
		}
	}

	var before []string
	var script []string
	var after []string

	for _, val := range job.Steps {

		if val.Name == "before_script" {
			before = val.Script
			continue
		}

		if val.Name == "script" {
			script = val.Script
			continue
		}

		if val.Name == "after_script" {
			after = val.Script
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
		if err = git("update-ref", "refs/"+targetName+"/"+sourceID, "HEAD"); err != nil {
			return err
		}
	}

	return err
}

func handleScript(data []string, trace io.Writer) error {

	var r = strings.NewReader(strings.Join(data, "\n"))
	var cmd = exec.Command(config.Shell)
	cmd.Stdin = r
	cmd.Stdout = trace
	cmd.Stderr = trace
	cmd.Dir = projDir
	return cmd.Run()
}

func getRefSHA(target string) string {

	var f, _ = os.Open(projDir + "/.git/refs/remotes/origin/" + target)
	if f == nil {
		return target
	}

	var data = make([]byte, 32)
	f.Read(data)
	f.Close()

	return string(data)
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
