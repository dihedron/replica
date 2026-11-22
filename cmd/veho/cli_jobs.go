package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/internal/client"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// JobCommand is "veho job".
type JobCommand struct {
	Submit JobSubmitCommand `command:"submit" description:"Submit a job"`
	Show   JobShowCommand   `command:"show" description:"Show a job"`
	List   JobListCommand   `command:"list" description:"List jobs"`
	Wait   JobWaitCommand   `command:"wait" description:"Wait until a job settles: exit 0 if it succeeded, 3 if it failed, 4 if it was cancelled"`
	Watch  JobWatchCommand  `command:"watch" description:"Print a job each time it changes, until it settles"`
	Cancel JobCancelCommand `command:"cancel" description:"Cancel a job"`
	Retry  JobRetryCommand  `command:"retry" description:"Retry the failed transfers of a failed job"`
}

// jobArg is a job's ID as the command's argument.
type jobArg struct {
	Args struct {
		ID string `positional-arg-name:"job" required:"true"`
	} `positional-args:"true"`
}

// JobSubmitCommand is "veho job submit".
type JobSubmitCommand struct {
	apiOptions
	Source         string   `long:"source" required:"true" description:"The source: <endpoint>:<path>"`
	PolicyFile     string   `long:"policy-file" description:"An inline policy document (- for standard input)"`
	Policy         string   `long:"policy" description:"A named policy: <name>[@<version>]"`
	Labels         []string `long:"label" description:"A label key=value (repeatable)"`
	IdempotencyKey string   `long:"idempotency-key" description:"Makes a repeated submission return the first job"`
}

// Execute submits the job and prints it.
func (cmd *JobSubmitCommand) Execute([]string) error {
	endpoint, path, ok := strings.Cut(cmd.Source, ":")
	if !ok || endpoint == "" || path == "" {
		return fmt.Errorf("--source %q: want <endpoint>:<path>", cmd.Source)
	}
	req := &vehov1.CreateJobRequest{Source: &vehov1.ObjectRef{Endpoint: endpoint, Path: path}, IdempotencyKey: cmd.IdempotencyKey,
		Labels: map[string]string{}}
	for _, l := range cmd.Labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return fmt.Errorf("--label %q: want key=value", l)
		}
		req.Labels[k] = v
	}
	switch {
	case (cmd.PolicyFile == "") == (cmd.Policy == ""):
		return errors.New("give --policy-file or --policy")
	case cmd.PolicyFile != "":
		st, err := structFile(cmd.PolicyFile)
		if err != nil {
			return err
		}
		req.Policy = st
	default:
		name, version, _ := strings.Cut(cmd.Policy, "@")
		ref := &vehov1.PolicyRef{Name: name}
		if version != "" {
			v, err := strconv.Atoi(version)
			if err != nil {
				return fmt.Errorf("--policy %q: want <name>[@<version>]", cmd.Policy)
			}
			ref.Version = int32(v) //nolint:gosec // a small version
		}
		req.PolicyRef = ref
	}
	return cmd.show("veho.v1.JobService.CreateJob", req, &vehov1.Job{})
}

// structFile reads a JSON object as a Struct.
func structFile(path string) (*structpb.Struct, error) {
	b, err := readInput(path)
	if err != nil {
		return nil, err
	}
	st := &structpb.Struct{}
	if err := protojson.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return st, nil
}

// JobShowCommand is "veho job show".
type JobShowCommand struct {
	apiOptions
	jobArg
}

// Execute prints the job.
func (cmd *JobShowCommand) Execute([]string) error {
	return cmd.show("veho.v1.JobService.GetJob", &vehov1.GetJobRequest{Id: cmd.Args.ID}, &vehov1.Job{})
}

// JobListCommand is "veho job list".
type JobListCommand struct {
	apiOptions
	State  string   `long:"state" description:"Only jobs in this state"`
	Since  string   `long:"since" description:"Only jobs created since (RFC 3339)"`
	Source string   `long:"source-endpoint" description:"Only jobs from this endpoint"`
	Labels []string `long:"label" description:"Only jobs with the label key=value (repeatable)"`
	Limit  int32    `long:"limit" description:"At most this many (default 100)"`
	Cursor string   `long:"cursor" description:"The next page, from a previous list"`
}

// Execute prints the jobs.
func (cmd *JobListCommand) Execute([]string) error {
	req := &vehov1.ListJobsRequest{State: cmd.State, SourceEndpoint: cmd.Source, Limit: cmd.Limit, Cursor: cmd.Cursor, Label: map[string]string{}}
	if cmd.Since != "" {
		t, err := time.Parse(time.RFC3339, cmd.Since)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		req.Since = timestamppb.New(t)
	}
	for _, l := range cmd.Labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return fmt.Errorf("--label %q: want key=value", l)
		}
		req.Label[k] = v
	}
	return cmd.show("veho.v1.JobService.ListJobs", req, &vehov1.ListJobsResponse{}, "id", "state", "created_at", "copies", "labels")
}

// JobWaitCommand is "veho job wait".
type JobWaitCommand struct {
	apiOptions
	jobArg
	MaxWait time.Duration `long:"max-wait" description:"Give up after this long (exit 5); default never"`
}

// Execute waits with the API's long poll until the job settles.
func (cmd *JobWaitCommand) Execute([]string) error {
	c, err := cmd.client()
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	m, err := client.Method("veho.v1.JobService.GetJob")
	if err != nil {
		return err
	}
	start, etag := time.Now(), ""
	var last *vehov1.Job
	for {
		if cmd.MaxWait > 0 && time.Since(start) > cmd.MaxWait {
			return &exitError{code: 5, msg: "the job did not settle within --max-wait"}
		}
		j := &vehov1.Job{}
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		var headers []string
		if etag != "" {
			headers = []string{"if-none-match", etag}
		}
		err := c.Call(ctx, m, &vehov1.GetJobRequest{Id: cmd.Args.ID, Wait: 30}, j, headers...)
		cancel()
		switch {
		case errors.Is(err, client.ErrNotModified):
			continue
		case err != nil:
			return err
		}
		last, etag = j, etagOf(j)
		if last.GetSettledAt() != nil {
			break
		}
	}
	if err := cmd.print(last); err != nil {
		return err
	}
	switch last.GetState() {
	case "SUCCEEDED":
		return nil
	case "CANCELLED":
		return &exitError{code: 4, msg: "the job was cancelled"}
	default:
		return &exitError{code: 3, msg: "the job " + strings.ToLower(last.GetState())}
	}
}

// etagOf is the server's ETag of a job.
func etagOf(j *vehov1.Job) string { return vehov1.ETag(j) }

// JobWatchCommand is "veho job watch".
type JobWatchCommand struct {
	apiOptions
	jobArg
}

// Execute prints the job each time it changes.
func (cmd *JobWatchCommand) Execute([]string) error {
	c, err := cmd.client()
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	m, err := client.Method("veho.v1.JobService.WatchJob")
	if err != nil {
		return err
	}
	return c.Stream(context.Background(), m, &vehov1.JobRef{Id: cmd.Args.ID}, func() proto.Message { return &vehov1.Job{} }, func(msg proto.Message) error {
		j := msg.(*vehov1.Job)
		if cmd.Output == "json" {
			return cmd.print(j)
		}
		_, err := fmt.Fprintf(cmd.stdout(), "%s %s copies %d/%d progress %.0f%%\n", time.Now().Format(time.TimeOnly), j.GetState(),
			j.GetCopies().GetDone(), j.GetCopies().GetTotal(), 100*j.GetProgress())
		return err
	})
}

// JobCancelCommand is "veho job cancel".
type JobCancelCommand struct {
	apiOptions
	jobArg
}

// Execute cancels the job.
func (cmd *JobCancelCommand) Execute([]string) error {
	return cmd.show("veho.v1.JobService.CancelJob", &vehov1.JobRef{Id: cmd.Args.ID}, &vehov1.Job{})
}

// JobRetryCommand is "veho job retry".
type JobRetryCommand struct {
	apiOptions
	jobArg
}

// Execute retries the job.
func (cmd *JobRetryCommand) Execute([]string) error {
	return cmd.show("veho.v1.JobService.RetryJob", &vehov1.JobRef{Id: cmd.Args.ID}, &vehov1.Job{})
}
