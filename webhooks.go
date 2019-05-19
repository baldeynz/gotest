package main

import (
	"bytes"
	"cloud.google.com/go/pubsub"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/go-github/github"
	valid "github.com/asaskevich/govalidator"
	vault "github.com/hashicorp/vault/api"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"time"
)

//Vars set by environment vars
var (
	projectID         string
	retryCount        int
	retryInterval     time.Duration
	topicName         string
	jenkinsWebhookUrl string
	subName           string
	secretPath        string
	vaultAddress      string
	vaultRole         string
)

const (
	invoke   = "token"
	k8Token  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	K8CaCert = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
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

	topicName = os.Getenv("TOPIC_NAME")
	if topicName == "" {
		logger.Fatal("Failed to get TOPIC_NAME")
	}

	jenkinsWebhookUrl = os.Getenv("JENKINS_URL")
	if jenkinsWebhookUrl == "" {
		logger.Fatal("Failed to get JENKINS_URL")
	}

	if !valid.IsURL(jenkinsWebhookUrl) {
		logger.Fatal("JENKINS_URL is invalid")
	}

	subName = os.Getenv("SUBSCRIPTION_NAME")
	if subName == "" {
		logger.Fatal("Failed to get SUBSCRIPTION_NAME")
	}

	secretPath = os.Getenv("VAULT_SECRET_PATH")
	if secretPath == "" {
		logger.Fatal("Failed to get VAULT_SECRET_PATH")
	}

	vaultAddress = os.Getenv("VAULT_ADDRESS")
	if vaultAddress == "" {
		logger.Fatal("Failed to get VAULT_ADDRESS")
	}

	vaultAddress = os.Getenv("VAULT_ADDRESS")
	if vaultAddress == "" {
		logger.Fatal("Failed to get VAULT_ADDRESS")
	}

	vaultRole = os.Getenv("VAULT_ROLE")
	if vaultRole == "" {
		logger.Fatal("Failed to get VAULT_ROLE")
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

	// define a channel `eventChannel` it will send/receive hookEvents
	eventChannel := make(chan hookEvent, 100)

	// fire up a new goroutine in the background for processing hook events
	go hookProcessor(eventChannel)

	srv := &http.Server{
		Addr:         ":8090",
		Handler:      http.HandlerFunc(processRequest(eventChannel)),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	//srv.HandleFunc("/webhooks", WebhookHandler)
	logger.Info("Server Started")
	logger.Fatal(srv.ListenAndServe())
}

type hookEvent struct {
	payload     []byte
	gitHubEvent string
	invokeKey   string
}

// hookProcessor takes the receive side of a channel
func hookProcessor(eventCh <-chan hookEvent) {
	// Ranging over a channel will read events sent to the channell
	// or block if there's no events to read.
	for event := range eventCh {
		logger.WithFields(log.Fields{"event-type": event.gitHubEvent}).Info("Recieved a new event to process")

		//validate we can parse the webhook
		gEvent, err := github.ParseWebHook(event.gitHubEvent, event.payload)
		if err != nil {
			logError(err,"Error process parsing incoming webhook")
		} else {
			switch gEvent.(type) {
			case *github.PushEvent:
				//try and send on to Jenkins
				webhookSenderr := retry(retryCount, retryInterval, func() (err error) {
					jenkinsStatus, err :=  sendToJenkins(event.payload, event.gitHubEvent, event.invokeKey)
					if err != nil {
						logError(err,jenkinsStatus)
					}
					return
				})
				//if jenkins send/retries are exhausted then we need to add the X-Github-Event and Token to the message and send to Topic
				if webhookSenderr != nil {
					logger.WithFields(log.Fields{"error": webhookSenderr}).Error("Something went wrong with webhook forward to Jenkins. Sending to topic")
					contentPlus, constructErr := constructPubSubMsg(event.payload, event.gitHubEvent, event.invokeKey)
					if constructErr != nil {
						logError(constructErr,"Pubsub msg construction error")
					}
					sendToTopic(contentPlus)
				} else { //log successful Jenkins send
					logger.WithFields(log.Fields{"jenkins-url": jenkinsWebhookUrl + "?token=" + event.invokeKey}).Info("Webhook forwarded to Jenkins")
				}
			default:
				logger.WithFields(log.Fields{"event-type": event.gitHubEvent}).Info("Currently we ignore this event-type")
			}
		}
	}
}

func processRequest(eventCh chan hookEvent) http.HandlerFunc {
	// Return a http.HandlerFunc that implements the handler functionality
	return func(w http.ResponseWriter, r *http.Request) {
		//Check to see if we have a relevant invoke query
		keys, ok := r.URL.Query()[invoke]
		if !ok || len(keys[0]) < 1 {
			logger.Error("invalid query")
			logger.Info(r.URL.RawQuery)
			http.Error(w, "Error validating request", http.StatusBadRequest)
			return
		}
		logger.WithFields(log.Fields{"token": keys[0]}).Info("Received webhook")

		//validate that our secret key matches what is in the headers
		content, err := github.ValidatePayload(r, []byte(secret))
		if err != nil {
			logError(err,"Error validating request body")
			http.Error(w, "Error validating request body", http.StatusBadRequest)
			return
		}
		// Instantiate a hookEvent{} and send it to the channel
		eventCh <- hookEvent{
			payload:     content,
			gitHubEvent: r.Header.Get("X-Github-Event"),
			invokeKey:   keys[0],
		}

		// Return status code and we're done
		w.WriteHeader(http.StatusAccepted)
		r.Body.Close()
	}
}

//returns content with X-Github-Event and Token-Path added to it suitable for sending to topic
func constructPubSubMsg(content []byte, eventType string, token string) (msg []byte, err error) {
	var readContent map[string]interface{}
	err = json.Unmarshal([]byte(content), &readContent)
	if err != nil {
    	logError(err,"Json unmarshaling error")
		return nil, err
	}
	readContent["X-Github-Event"] = eventType
	readContent["Token-Path"] = token
	contentPlus, jsonMarsherr := json.Marshal(readContent)
	if jsonMarsherr != nil {
		logError(jsonMarsherr,"Json marshaling error")
		return nil, jsonMarsherr
	}
	return contentPlus, nil
}

func sendToJenkins(content []byte, githubEvent string, invokeToken string) (string, error) {
	//Send the request on to the Jenkins Url
	req, err := http.NewRequest("POST", jenkinsWebhookUrl + "?token=" + invokeToken, bytes.NewBuffer(content))
	if err != nil {
		return "HTTP Request Error", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Github-Event", githubEvent)

	client := &http.Client{}
	response, err := client.Do(req)
	if err != nil {
		return "Jenkins Transport Error", err
	}
	defer response.Body.Close()

	//test we are getting the correct status code
	if response.StatusCode != http.StatusOK {
		return jenkinsWebhookUrl + "?token=" + invokeToken + " Response Error", errors.New(strconv.Itoa(response.StatusCode))
	}

	return "OK", nil
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
		logger.WithFields(log.Fields{"error": err, "attempts": i + 1}).Error("Retrying attempt after error")

	}
	return fmt.Errorf("after %d attempts, last error: %s", attempts, err)
}

func initPubClient() {
	// err is pre-declared to avoid shadowing client.
	var err error
	client, err = pubsub.NewClient(ctx, projectID)
	if err != nil {
		logError(err,"Failed to initialize pub sub client")
	}
}

func getVaultSecret() (secret string) {
	jwt, err := ioutil.ReadFile(k8Token)
	logFatal(err, "JWT Token")

	//Set Vault Address
	vaultConfig := &vault.Config{
		Address: vaultAddress,
	}

	//Set CA Cert location
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
