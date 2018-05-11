// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// These tests utilize a recorder to replay AWS endpoint hits from golden files.
// Golden files are used if -short is passed to `go test`.
// If -short is not passed, the recorder will make a call to AWS and save a new golden file.
package paramstore

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/dnaeon/go-vcr/cassette"
	"github.com/dnaeon/go-vcr/recorder"
	"github.com/google/go-cloud/runtimevar"
	"github.com/google/go-cmp/cmp"
)

const region = "us-east-2"

// TestWriteReadDelete attempts to write, read and then delete parameters from Parameter Store.
// This test can't be broken up into separate Test(Write|Read|Delete) tests
// because the only way to make the test hermetic is for the test to be able
// to perform all the functions.
func TestWriteReadDelete(t *testing.T) {
	macbeth, err := ioutil.ReadFile("testdata/macbeth.txt")
	if err != nil {
		t.Fatalf("error reading long data file: %v", err)
	}

	tests := []struct {
		name, paramName, value string
		wantWriteErr           bool
	}{
		{
			name:      "Good param name and value should pass",
			paramName: "test-good-param",
			value:     "Jolly snowman to test Unicode handling: ☃️",
		},
		{
			// Param names that aren't letters, numbers or common symbols can't be created.
			name:         "Bad param name should fail",
			paramName:    "test-bad-param-with-snowman-☃️",
			value:        "snowman",
			wantWriteErr: true,
		},
		{
			name:         "Good param name with an empty value should fail",
			paramName:    "test-good-empty-param",
			wantWriteErr: true,
		},
		{
			name:         "Empty param name should fail",
			paramName:    "",
			value:        "empty",
			wantWriteErr: true,
		},
		{
			name: "Long param name should fail",
			paramName: `
		Hodor. Hodor HODOR hodor, hodor hodor, hodor. Hodor hodor hodor hodor?!
		Hodor, hodor. Hodor. Hodor, HODOR hodor, hodor hodor; hodor hodor hodor,
		hodor. Hodor hodor, hodor, hodor hodor. Hodor. Hodor hodor... Hodor hodor
		hodor hodor! Hodor. Hodor hodor hodor - hodor, hodor, hodor hodor. Hodor.
		Hodor hodor hodor hodor hodor - hodor? Hodor HODOR hodor, hodor hodor
		hodor hodor?! Hodor. Hodor hodor... Hodor hodor hodor?
		`,
			value:        "HODOOORRRRR!",
			wantWriteErr: true,
		},
		{
			// AWS documents that 4096 is the maximum size of a parameter value.
			// Interestingly, it appears to accept more, but it's not obvious how
			// much that is. Test that it at least works for 4096.
			name:      "Good value of 4096 should pass",
			paramName: "test-good-size-value",
			value:     string(macbeth)[:4096],
		},
		{
			name:         "Bad value of a really long parameter should fail",
			paramName:    "test-bad-size-value",
			value:        string(macbeth),
			wantWriteErr: true,
		},
	}

	sess, done := newSession(t, "write_read_delete")
	defer done()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := writeParam(sess, tc.paramName, tc.value)
			switch {
			case err != nil && !tc.wantWriteErr:
				t.Fatalf("got error %v; want nil", err)
			case err == nil && tc.wantWriteErr:
				t.Errorf("got nil error; want error")
			case err != nil && tc.wantWriteErr:
				// Writing has failed as expected, continue other tests.
				return
			}
			defer func() {
				if err := deleteParam(sess, tc.paramName); err != nil {
					t.Log(err)
				}
			}()

			p, err := readParam(sess, tc.paramName, -1)
			switch {
			case err != nil:
				t.Errorf("got error %v; want nil", err)
			case p.name != tc.paramName:
				t.Errorf("got %s; want %s", p.name, tc.paramName)
			case p.value != tc.value:
				t.Errorf("got %s; want %s", p.value, tc.value)
			}
		})
	}
}

func TestInitialWatch(t *testing.T) {
	tests := []struct {
		name, param                 string
		ctx                         context.Context
		waitTime                    time.Duration
		wantNewVarErr, wantWatchErr bool
	}{
		{
			name:     "Good param should return OK",
			param:    "test-watch-initial",
			ctx:      context.Background(),
			waitTime: time.Second,
		},
		{
			name:          "Bad wait time should fail",
			param:         "test-bad-wait-time",
			ctx:           context.Background(),
			waitTime:      -1,
			wantNewVarErr: true,
		},
		{
			name:  "A canceled context should fail",
			param: "test-canceled-context",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			}(),
			wantWatchErr: true,
		},
	}

	sess, done := newSession(t, "watch_initial")
	defer done()

	for _, tc := range tests {
		const want = "foobar"
		t.Run(tc.name, func(t *testing.T) {
			if _, err := writeParam(sess, tc.param, want); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := deleteParam(sess, tc.param); err != nil {
					t.Fatal(err)
				}
			}()

			variable, err := NewClient(tc.ctx, sess).NewVariable(tc.ctx, tc.param, runtimevar.StringDecoder, &WatchOptions{WaitTime: tc.waitTime})
			switch {
			case err != nil && !tc.wantNewVarErr:
				t.Fatal(err)
			case err == nil && tc.wantNewVarErr:
				t.Fatalf("got %+v; want error", variable)
			case err != nil && tc.wantNewVarErr:
				// Got error as expected.
				return
			}

			got, err := variable.Watch(tc.ctx)
			switch {
			case err != nil && !tc.wantWatchErr:
				t.Fatal(err)
			case err == nil && tc.wantWatchErr:
				t.Errorf("got %+v; want error", got)
			case err == nil && !tc.wantWatchErr && got.Value != want:
				t.Errorf("got %v; want %v", got.Value, want)
			}
		})
	}
}

func TestWatchObservesChange(t *testing.T) {
	tests := []struct {
		name, param, firstValue, secondValue string
		wantErr                              bool
	}{
		{
			name:        "Good param should flip OK",
			param:       "test-watch-observes-change",
			firstValue:  "foo",
			secondValue: "bar",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess, done := newSession(t, "watch_change")
			defer done()

			if _, err := writeParam(sess, tc.param, tc.firstValue); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := deleteParam(sess, tc.param); err != nil {
					t.Fatal(err)
				}
			}()

			ctx := context.Background()
			variable, err := NewClient(ctx, sess).NewVariable(ctx, tc.param, runtimevar.StringDecoder, &WatchOptions{WaitTime: time.Second})
			got, err := variable.Watch(ctx)
			switch {
			case err != nil:
				t.Fatal(err)
			case got.Value != tc.firstValue:
				t.Errorf("want %v; got %v", tc.firstValue, got.Value)
			}

			// Write again and see that watch sees the new value.
			if _, err := writeParam(sess, tc.param, tc.secondValue); err != nil {
				t.Fatal(err)
			}

			got, err = variable.Watch(ctx)
			switch {
			case err != nil:
				t.Fatal(err)
			case got.Value != tc.secondValue:
				t.Errorf("want %v; got %v", tc.secondValue, got.Value)
			}
		})
	}
}

func TestJSONDecode(t *testing.T) {
	type Message struct {
		Name, Text string
	}

	var tests = []struct {
		name, param, json string
		want              []*Message
	}{
		{
			name:  "Valid JSON should be unmarshaled correctly",
			param: "test-json-decode",
			json: `[
{"Name": "Ed", "Text": "Knock knock."},
{"Name": "Sam", "Text": "Who's there?"}
]`,
			want: []*Message{{Name: "Ed", Text: "Knock knock."}, {Name: "Sam", Text: "Who's there?"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess, done := newSession(t, "decoder")
			defer done()

			if _, err := writeParam(sess, tc.param, tc.json); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := deleteParam(sess, tc.param); err != nil {
					t.Fatal(err)
				}
			}()

			ctx := context.Background()
			var jsonDataPtr []*Message
			variable, err := NewClient(ctx, sess).NewVariable(ctx, tc.param, runtimevar.NewDecoder(jsonDataPtr, runtimevar.JSONDecode), &WatchOptions{WaitTime: time.Second})
			got, err := variable.Watch(ctx)

			switch {
			case err != nil:
				t.Error(err)
			case !cmp.Equal(got.Value.([]*Message), tc.want):
				t.Errorf("got %+v, want %+v", got.Value, tc.want)
			}
		})
	}
}

func TestDeleteNonExistentParam(t *testing.T) {
	// TODO(cflewis): Implement :)
}

func newRecorder(t *testing.T, filename string) (r *recorder.Recorder, done func()) {
	path := filepath.Join("testdata", filename)
	t.Logf("Golden file is at %v", path)

	mode := recorder.ModeReplaying
	if !testing.Short() {
		t.Logf("Recording into golden file")
		mode = recorder.ModeRecording
	}
	r, err := recorder.NewAsMode(path, mode, nil)
	if err != nil {
		t.Fatalf("unable to record: %v", err)
	}

	// Use a custom matcher as the default matcher looks for URLs and methods,
	// which Amazon overloads as it isn't RESTful.
	// Sequencing is added to the requests when the cassette is saved, which
	// allows for differentiating GETs which otherwise look identical.
	last := -1
	r.SetMatcher(func(r *http.Request, i cassette.Request) bool {
		if r.Body == nil {
			return false
		}
		var b bytes.Buffer
		if _, err := b.ReadFrom(r.Body); err != nil {
			t.Fatal(err)
		}
		r.Body = ioutil.NopCloser(&b)

		seq, err := strconv.Atoi(i.Headers.Get("X-Gocloud-Seq"))
		if err != nil {
			t.Fatal(err)
		}

		t.Logf("Targets: %v | %v == %v\n", r.Header.Get("X-Amz-Target"), i.Headers.Get("X-Amz-Target"), r.Header.Get("X-Amz-Target") == i.Headers.Get("X-Amz-Target"))
		t.Logf("URLs: %v | %v == %v\n", r.URL.String(), i.URL, r.URL.String() == i.URL)
		t.Logf("Methods: %v | %v == %v\n", r.Method, i.Method, r.Method == i.Method)
		t.Logf("Bodies:\n%v\n%v\n==%v\n", b.String(), i.Body, b.String() == i.Body)

		if r.Header.Get("X-Amz-Target") == i.Headers.Get("X-Amz-Target") &&
			r.URL.String() == i.URL &&
			r.Method == i.Method &&
			b.String() == i.Body &&
			seq > last {
			last = seq
			return true
		}

		return false
	})

	return r, func() { r.Stop(); fixHeaders(path) }
}

func newSession(t *testing.T, filename string) (sess *session.Session, done func()) {
	r, done := newRecorder(t, filename)
	defer done()

	client := &http.Client{
		Transport: r,
	}

	// Provide fake creds if running in replay mode.
	var creds *credentials.Credentials
	if testing.Short() {
		creds = credentials.NewStaticCredentials("FAKE_ID", "FAKE_SECRET", "FAKE_TOKEN")
	}

	sess, err := session.NewSession(aws.NewConfig().WithHTTPClient(client).WithRegion(region).WithCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}

	return sess, done
}

// fixHeaders removes *potentially* sensitive information from a cassette,
// and adds sequencing to the requests to differentiate Amazon calls, as they
// aren't timestamped.
// Note that sequence numbers should only be used for otherwise identical matches.
func fixHeaders(filepath string) error {
	c, err := cassette.Load(filepath)
	if err != nil {
		return fmt.Errorf("unable to load cassette, do not commit to repository: %v", err)
	}

	c.Mu.Lock()
	for i, action := range c.Interactions {
		action.Request.Headers.Set("X-Gocloud-Seq", strconv.Itoa(i))
		action.Request.Headers.Del("Authorization")
		action.Response.Headers.Del("X-Amzn-Requestid")
	}
	c.Mu.Unlock()
	c.Save()

	return nil
}

func writeParam(p client.ConfigProvider, name, value string) (int64, error) {
	svc := ssm.New(p)
	resp, err := svc.PutParameter(&ssm.PutParameterInput{
		Name:      aws.String(name),
		Type:      aws.String("String"),
		Value:     aws.String(value),
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return -1, err
	}

	return *resp.Version, err
}

func deleteParam(p client.ConfigProvider, name string) error {
	svc := ssm.New(p)
	_, err := svc.DeleteParameter(&ssm.DeleteParameterInput{Name: aws.String(name)})
	return err
}