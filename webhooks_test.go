package main

import (
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"log"
	"testing"
)

//Tests that the constructPubSubMsg func returns with the correct headers added
func TestConstructPubSubMsg(t *testing.T) {
	type W struct{}

	type WebhookPlus struct {
		XGithubEvent string `json:"X-Github-Event"`
		TokenPath    string `json:"Token-Path"`
	}
	eventType := "push"
	token := "experiment"

	contentTest, err := json.Marshal(W {})
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
