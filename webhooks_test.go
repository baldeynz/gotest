package main

import (
	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"context"
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

const (
	eventType = "push"
	token     = "experiment"
	retries   = 2
)

func TestLoadVars(t *testing.T) {
	ev := map[string]string{
		"GCP_PROJECT":       "a-project",
		"JENKINS_URL":       "https://jenkins.jenkins:8080",
		"VAULT_SECRET_PATH": "secret/blah",
		"RETRY_INTERVAL":    "5",
		"RETRY_COUNT":       "2",
		"SUBSCRIPTION_NAME": "sub",
		"TOPIC_NAME":        "topic",
		"VAULT_ADDRESS":     "https://anaddress:8200",
		"VAULT_ROLE":        "role",
	}
	for k, v := range ev {
		os.Setenv(k, v)
	}
	envars := loadVars()
	assert.Equal(t, envars.ProjectID, ev["GCP_PROJECT"])
}

func TestSendToTopic(t *testing.T) {
	ctx := context.Background()
	// Start a fake server running locally.
	srv := pstest.NewServer()
	defer srv.Close()
	// Connect to the server without using TLS.
	conn, err := grpc.Dial(srv.Addr, grpc.WithInsecure())
	if err != nil {
		t.Fatalf("failed to dial pstest server: %v", err)
	}
	defer conn.Close()
	// Use the connection when creating a pubsub client.
	client, err := pubsub.NewClient(ctx, "project", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatalf("failed creating pubsub client: %v", err)
	}
	defer client.Close()

	top, err := client.CreateTopic(ctx, "testTopic")
	if err != nil {
		t.Fatalf("failed creating topic: %v", err)
	}
	defer top.Stop()

	//No error is returned from sendToTopic
	assert.Nil(t, sendToTopic(ctx, client, []byte("Test Message"), "testTopic"))

}


//Tests that the constructPubSubMsg func returns with the correct headers added
func TestConstructPubSubMsg(t *testing.T) {
	type W struct{}

	type WebhookPlus struct {
		XGithubEvent string `json:"X-Github-Event"`
		TokenPath    string `json:"Token-Path"`
	}

	contentTest, err := json.Marshal(W{})
	if err != nil {
		t.Fatalf("failed json marshal : %v", err)
	}
	output, _ := constructPubSubMsg(contentTest, eventType, token)
	var cReturned WebhookPlus
	err = json.Unmarshal(output, &cReturned)
	if err != nil {
		t.Fatalf("failed json unmarshal : %v", err)
	}

	assert.Equal(t, eventType, cReturned.XGithubEvent)
	assert.Equal(t, token, cReturned.TokenPath)
}

//Tests sendtoJenkins func
func TestSendToJenkins(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		// Test request headers
		assert.Equal(t, req.Header.Get("X-Github-Event"), eventType)
		assert.Equal(t, req.Header.Get("Content-Type"), "application/json")
		// Send response
		rw.Write([]byte(`OK`))
	}))
	defer server.Close()

	err := sendToJenkins(nil, eventType, server.URL)
	if err != nil {
		t.Fatalf("send to jenkins error : %v", err)
	}
}

//Tests retry function with bad path
func TestRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		// Send response
		rw.WriteHeader(400)
		rw.Write([]byte(`OK`))
	}))
	defer server.Close()

	err := retry(retries, 2*time.Second, func() (err error) {
		jenkinsErr := sendToJenkins(nil, eventType, server.URL+"/badpath")
		if jenkinsErr != nil {
			assert.Equal(t, "400", jenkinsErr.Error())
			return jenkinsErr
		}
		return
	})

	if err != nil {
		assert.Equal(t, "after 2 attempts, last error: 400", err.Error())
	}
}
