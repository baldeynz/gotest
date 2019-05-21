package main

import (
	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"context"
	"encoding/json"
	"errors"
	"github.com/stretchr/testify/assert"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	eventType = "push"
	token     = "experiment"
	retries   = 2
)


func TestSendToTopic(t *testing.T) {
	ctx := context.Background()
	// Start a fake server running locally.
	srv := pstest.NewServer()
	defer srv.Close()
	// Connect to the server without using TLS.
	conn, err := grpc.Dial(srv.Addr, grpc.WithInsecure())
	if err != nil {
		// TODO: Handle error.
	}
	defer conn.Close()
	// Use the connection when creating a pubsub client.
	client, err := pubsub.NewClient(ctx, "project", option.WithGRPCConn(conn))
	if err != nil {
		log.Println(err)
	}
	defer client.Close()
    
    //No error is returned from sendToTopic
	assert.Nil(t, sendToTopic([]byte("Test Message"), client, "testTopic"))
	
}

func TestLogError(t *testing.T){
	logError(errors.New("Error"), "Test Error")
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
		log.Println(err)
	}
	output, _ := constructPubSubMsg(contentTest, eventType, token)
	var cReturned WebhookPlus
	err = json.Unmarshal(output, &cReturned)
	if err != nil {
		log.Println(err)
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
		log.Println(err)
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
		log.Println(err)
		assert.Equal(t, "after 2 attempts, last error: 400", err.Error())
	}
	//assert.Equal(t,"after 2 attempts, last error: 400",webhookSenderr.Error())
}
