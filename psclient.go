package main

import (
	"bytes"
	"cloud.google.com/go/pubsub"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	valid "github.com/asaskevich/govalidator"
	vault "github.com/hashicorp/vault/api"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	k8Token  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	K8CaCert = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// projectID is set from the GCP_PROJECT environment variable
var (
	projectID         string
	jenkinsWebhookUrl string
	retryCount        int
	secretPath        string
	subName           string
	topicName         string
	vaultAddress      string
	vaultRole         string
	retryInterval     time.Duration
)

// global logger
var logger = log.WithFields(log.Fields{"app": "webhooks"})

//vault secret to validate payload
var secret string

// client is a global Pub/Sub client, initialized once per instance.
var client *pubsub.Client
var ctx = context.Background()

func main() {
	//get required Env Vars and fail pod start if we dont
	projectID = os.Getenv("GCP_PROJECT")
	if projectID == "" {
		logger.Fatal("Failed to get GCP_PROJECT")
	}

	jenkinsWebhookUrl = os.Getenv("JENKINS_URL")
	if jenkinsWebhookUrl == "" {
		logger.Fatal("Failed to get JENKINS_URL")
	}

	if !valid.IsURL(jenkinsWebhookUrl) {
		logger.Fatal("JENKINS_URL is invalid")
	}

	secretPath = os.Getenv("VAULT_SECRET_PATH")
	if secretPath == "" {
		logger.Fatal("Failed to get VAULT_SECRET_PATH")
	}

	vaultAddress = os.Getenv("VAULT_ADDRESS")
	if vaultAddress == "" {
		logger.Fatal("Failed to get VAULT_ADDRESS")
	}

	if !valid.IsURL(vaultAddress) {
		logger.Fatal("VAULT_ADDRESS is invalid")
	}

	vaultRole = os.Getenv("VAULT_ROLE")
	if vaultRole == "" {
		logger.Fatal("Failed to get VAULT_ROLE")
	}

	subName = os.Getenv("SUBSCRIPTION_NAME")
	if subName == "" {
		logger.Fatal("Failed to get SUBSCRIPTION_NAME")
	}

	topicName = os.Getenv("TOPIC_NAME")
	if topicName == "" {
		logger.Fatal("Failed to get TOPIC_NAME")
	}

	retryAttempts, err := strconv.Atoi(os.Getenv("RETRY_COUNT"))
	if err != nil {
		logger.Fatal("RETRY_COUNT is invalid")
	}
	retryCount = retryAttempts

	retryInt, err := strconv.Atoi(os.Getenv("RETRY_INTERVAL"))
	if err != nil {
		logger.Fatal("RETRY_INTERVAL is invalid")
	}
	retryInterval = time.Duration(retryInt) * time.Second
	// Log as JSON instead of the default ASCII formatter.
	log.SetFormatter(&log.JSONFormatter{})
	// Output to stdout instead of the default stderr
	log.SetOutput(os.Stdout)
	// Only log the warning severity or above.
	log.SetLevel(log.InfoLevel)

	//we call getVaultSecret here so we dont start the pod if we fail it
	secret = getVaultSecret()

	initPubClient()

	// Start worker goroutine.
	err = subscribe()
	if err != nil {
		logger.WithFields(log.Fields{"error": err}).Error("Failed to run subscribe routine")
	}

}

func subscribe() error {
	var mu sync.Mutex
	sub := client.Subscription(subName)
	ctx, cancel := context.WithCancel(ctx)
	err := sub.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
		msg.Ack()
		logger.WithFields(log.Fields{"message-id": msg.ID}).Info("Got message: Deconstructing...")
		payload, githubEventheader, invokeToken, err := constructHttpMsg(msg.Data)
		//calculate the signature based on the content and secret
		gitXheader := "sha1=" + ComputeHmac(string(payload), secret)
		logger.WithFields(log.Fields{"X-Hub-Signature": gitXheader})

		if err != nil {
			logger.WithFields(log.Fields{"error": err}).Error("Error Deconstructing msg")
		}

		webhookSenderr := retry(retryCount, retryInterval, func() (err error) {
			jenkinsStatus, err := sendToJenkins(payload, gitXheader, githubEventheader, invokeToken)
			if err != nil {
				logger.WithFields(log.Fields{"error": jenkinsStatus}).Error("Jenkins retry")
			}
			return
		})
		if webhookSenderr != nil {
			logger.Error("Webhook Service not available. Sending to Topic")
			contentPlus, constructErr := constructPubSubMsg(payload, githubEventheader, invokeToken)
			if constructErr != nil {
				logger.WithFields(log.Fields{"error": constructErr}).Fatal("Pubsub msg construction error")
			}
			sendToTopic(contentPlus)
		}

		logger.WithFields(log.Fields{"jenkins-url": jenkinsWebhookUrl}).Info("Webhook forwarded successfully")

		mu.Lock()
		defer mu.Unlock()
		defer cancel() //cancel ctx as soon as we return
	})
	if err != nil {
		return err
	}
	return nil
}

func initPubClient() {
	// err is pre-declared to avoid shadowing client.
	var err error

	// client is initialized with context.Background()
	client, err = pubsub.NewClient(ctx, projectID)
	if err != nil {
		logger.WithFields(log.Fields{"error": err}).Error("Failed to initialize pub sub client")
	}

}

func constructHttpMsg(msg []byte) (content []byte, httpHeader string, invoke string, err error) {
	var readContent map[string]interface{}
	err = json.Unmarshal([]byte(msg), &readContent)
	gitHubEvent := fmt.Sprint(readContent["X-Github-Event"])
	invokeToken := fmt.Sprint(readContent["Token-Path"])
	delete(readContent, "X-Github-Event")
	contentPlus, jsonMarsherr := json.Marshal(readContent)
	if jsonMarsherr != nil {
		logger.WithFields(log.Fields{"Original Message": content}).Error("Json Marshalling")
		logger.WithFields(log.Fields{"error": jsonMarsherr}).Fatal("Something went wrong with json marshalling")
		return nil, "", "", jsonMarsherr
	}

	return contentPlus, gitHubEvent, invokeToken, nil

}

func sendToJenkins(content []byte, githubSig string, githubEventheader string, invokeToken string) (string, error) {
	logger.Info(jenkinsWebhookUrl + "?token=" + invokeToken)
	//Send the request on to teh Jenkins Url
	req, err := http.NewRequest("POST", jenkinsWebhookUrl+"?token="+invokeToken, bytes.NewBuffer(content))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature", githubSig)
	req.Header.Set("X-Github-Event", githubEventheader)

	client := &http.Client{}
	response, err := client.Do(req)
	if err != nil {
		return "Jenkins Transport Error", err
	}
	defer response.Body.Close()

	//test we are getting the correct status code
	if response.StatusCode != http.StatusAccepted {
		logger.WithFields(log.Fields{"response-code": response.Status}).Error("Non 202 Response Status Code")
		return "Incorrect Response Code", errors.New("Non 202 Response Status Code")
	}

	return "OK", nil
}

func retry(attempts int, sleep time.Duration, f func() error) (err error) {
	for i := 0; ; i++ {
		err = f()
		if err == nil {
			return
		}

		if i >= (attempts - 1) {
			break
		}

		time.Sleep(sleep)
		logger.WithFields(log.Fields{"attempts": attempts}).Info("Retry attempt")
	}
	return fmt.Errorf("after %d attempts, last error: %s", attempts, err)
}

func ComputeHmac(message string, secret string) string {
	key := []byte(secret)
	hash := hmac.New(sha1.New, key)
	hash.Write([]byte(message))
	return hex.EncodeToString(hash.Sum(nil))
}

func getVaultSecret() (secret string) {
	jwt, err := ioutil.ReadFile(k8Token)
	logFatal(err, "JWT Token")

	//Set Vault Address and CA Cert
	vaultConfig := &vault.Config{
		Address: vaultAddress,
	}

	tlsConfig := &vault.TLSConfig{
		CACert: K8CaCert,
	}

	tlserr := vaultConfig.ConfigureTLS(tlsConfig)
	logFatal(tlserr, "Vault TLS Config")

	client, err := vault.NewClient(vaultConfig)
	logFatal(err, "Vault Client Initiation")

	auth, err := client.Logical().Write("auth/kubernetes/login", map[string]interface{}{"jwt": string(jwt), "role": vaultRole})
	logFatal(err, "Vault Auth")

	client.SetToken(auth.Auth.ClientToken)

	secretValues, err := client.Logical().Read(secretPath)
	logFatal(err, "Vault Get Secret")

	logger.Info("Vault Secret Successfully Retrieved")
	return fmt.Sprint(secretValues.Data["value"])

}

//returns content with X-Github-Event and Token-Path added to it suitable for sending to topic
func constructPubSubMsg(content []byte, eventType string, token string) (msg []byte, err error) {
	var readContent map[string]interface{}
	err = json.Unmarshal([]byte(content), &readContent)
	readContent["X-Github-Event"] = eventType
	readContent["Token-Path"] = token
	contentPlus, jsonMarsherr := json.Marshal(readContent)
	if jsonMarsherr != nil {
		logger.WithFields(log.Fields{"error": jsonMarsherr}).Error("Json marshaling error")
		return nil, jsonMarsherr
	}
	return contentPlus, nil
}

func sendToTopic(c []byte) {
	//publish the message
	m := &pubsub.Message{
		Data: []byte(c),
	}

	id, err := client.Topic(topicName).Publish(ctx, m).Get(ctx)
	if err != nil {
		logger.WithFields(log.Fields{"topic": topicName, "error": err}).Error("Topic publish error")
		logger.WithFields(log.Fields{"message": string(c)}).Error("Topic publish error")
		return
	}
	logger.WithFields(log.Fields{"message-id": id}).Info("Message sent to topic successfully")
	return
}

func logFatal(e error, desc string) {
	if e != nil {
		logger.WithFields(log.Fields{"error": e}).Fatal(desc)
	}
}

func logError(e error, desc string) {
	if e != nil {
		logger.WithFields(log.Fields{"error": e}).Error(desc)
	}
}
