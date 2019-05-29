package main

import (
	"bytes"
	"cloud.google.com/go/pubsub"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/go-github/github"
	vault "github.com/hashicorp/vault/api"
	"go.uber.org/zap" //github.com/uber-go/zap currently has an issue
	"go.uber.org/zap/zapcore"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

const (
	invoke   = "token"
	K8Token  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	K8CACert = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// global logger
var logger = setLogger()

// client is a global Pub/Sub client, initialized once per instance.
var topic *pubsub.Topic 

func isValidURL(toTest string) bool {
    _, err := url.ParseRequestURI(toTest)
    if err != nil {
        return false
    } 
    return true   
}

//EnvironmentVars for predefined environment vars
type EnvironmentVars struct {
	ProjectID         string
	JenkinsWebhookURL string
	RetryCount        int
	RetryInterval     time.Duration
	SecretPath        string
	SubName           string
	TopicName         string
	VaultAddress      string
	VaultRole         string
}

//LoadVars Attemtp to load environment vars and fail if we cant
func loadVars() *EnvironmentVars {
	projectID := os.Getenv("GCP_PROJECT")
	if projectID == "" {
		logger.Fatal("Failed to get GCP_PROJECT")
	}

	jenkinsWebhookURL := os.Getenv("JENKINS_URL")
	if jenkinsWebhookURL == "" {
		logger.Fatal("Failed to get JENKINS_URL")
	}
	if !isValidURL(jenkinsWebhookURL) {
		logger.Fatal("JENKINS_URL is invalid")
	}

	secretPath := os.Getenv("VAULT_SECRET_PATH")
	if secretPath == "" {
		logger.Fatal("Failed to get VAULT_SECRET_PATH")
	}

	vaultAddress := os.Getenv("VAULT_ADDRESS")
	if vaultAddress == "" {
		logger.Fatal("Failed to get VAULT_ADDRESS")
	}

	if !isValidURL(vaultAddress) {
		logger.Fatal("VAULT_ADDRESS is invalid")
	}

	vaultRole := os.Getenv("VAULT_ROLE")
	if vaultRole == "" {
		logger.Fatal("Failed to get VAULT_ROLE")
	}

	subName := os.Getenv("SUBSCRIPTION_NAME")
	if subName == "" {
		logger.Fatal("Failed to get SUBSCRIPTION_NAME")
	}

	topicName := os.Getenv("TOPIC_NAME")
	if topicName == "" {
		logger.Fatal("Failed to get TOPIC_NAME")
	}

	retryAttempts, err := strconv.Atoi(os.Getenv("RETRY_COUNT"))
	if err != nil {
		logger.Fatal("RETRY_COUNT is invalid")
	}

	retryInt, err := strconv.Atoi(os.Getenv("RETRY_INTERVAL"))
	if err != nil {
		logger.Fatal("RETRY_INTERVAL is invalid")
	}

	return &EnvironmentVars{
		ProjectID:         projectID,
		JenkinsWebhookURL: jenkinsWebhookURL,
		RetryCount:        retryAttempts,
		RetryInterval:     time.Duration(retryInt) * time.Second,
		SecretPath:        secretPath,
		SubName:           subName,
		TopicName:         topicName,
		VaultAddress:      vaultAddress,
		VaultRole:         vaultRole,
	}
}


func setLogger() *zap.Logger {
 	atom := zap.NewAtomicLevel()
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "time"
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder

	logger := zap.New(zapcore.NewCore(
	    zapcore.NewJSONEncoder(encoderCfg),
	    zapcore.Lock(os.Stdout),
	    atom,
	))
	defer logger.Sync()

	return logger
}

func logInfo(msg string, key string, val string) {
	logger.Info(msg,
		zap.String(key, val),
		zap.String("app", "webhooks"),
	)
}

func logFatal(e error, desc string) {
	logger.Fatal(desc,
    	zap.Error(e),
    	zap.String("app", "webhooks"),
    )
}

func logError(e error, desc string) {
	logger.Error(desc,
		zap.Error(e),
		zap.String("app", "webhooks"),
    )
}

type hookEvent struct {
	payload     []byte
	gitHubEvent string
	invokeKey   string
}

// hookProcessor takes the receive side of a channel
func hookProcessor(ctx context.Context, client *pubsub.Client, envars *EnvironmentVars, eventCh <-chan hookEvent) {
	// Ranging over a channel will read events sent to the channell
	// or block if there's no events to read.
	for event := range eventCh {
		logInfo("Recieved a new event to process", "event-type",event.gitHubEvent)
		
		//validate we can parse the webhook
		gEvent, err := github.ParseWebHook(event.gitHubEvent, event.payload)
		if err != nil {
			logError(err,"Error process parsing incoming webhook")
		    continue
		}
		switch gEvent.(type) {
		case *github.PushEvent:
			//try and send on to Jenkins
			webhookSendErr := retry(envars.RetryCount, envars.RetryInterval, func() (err error) {
				JenkinsErr :=  sendToJenkins(event.payload, event.gitHubEvent, envars.JenkinsWebhookURL + "?token=" + event.invokeKey)
				if JenkinsErr != nil {
					logError(JenkinsErr, "Jenkins Transport Error")
					return JenkinsErr
				}
				return
			})
			//if jenkins send/retries are exhausted then we need to add the X-Github-Event and Token to the message and send to Topic
			if webhookSendErr != nil {
				logError(webhookSendErr,"Something went wrong with webhook forward to Jenkins. Sending to topic")
				contentPlus, constructErr := constructPubSubMsg(event.payload, event.gitHubEvent, event.invokeKey)
				if constructErr != nil {
					logError(constructErr,"Pubsub msg construction error")
				}
				
				err := sendToTopic(ctx, client, contentPlus, envars.TopicName)
				if err != nil {
					logFatal(err, "Could not publish message to topic")
				}
			} else { //log successful Jenkins send
				logInfo("Webhook forwarded to Jenkins", "jenkins-url",envars.JenkinsWebhookURL + "?token=" + event.invokeKey)
			}
		default:
			logInfo("Currently we ignore this event-type", "event-type", event.gitHubEvent)
		}
	}
}

func processRequest(secret string, eventCh chan hookEvent) http.HandlerFunc {
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
		logInfo("Received webhook", "token", keys[0])
	
		//validate that our secret key matches what is in the headers
		content, err := github.ValidatePayload(r, []byte(secret))
		if err != nil {
			logError(err,"Error validating request body")
			http.Error(w, "Error validating request body", http.StatusBadRequest)
			return
		}
		select {
			// Instantiate a hookEvent{} and send it to the channel unless its full
			case eventCh <- hookEvent{
					payload:     content,
					gitHubEvent: r.Header.Get("X-Github-Event"),
					invokeKey:   keys[0],
				}:
				// Return status code and we're done
				w.WriteHeader(http.StatusAccepted)
				r.Body.Close()
			default: 
				//buffer is full so send a 503
				w.WriteHeader(http.StatusServiceUnavailable)
				r.Body.Close()
        }		
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

func sendToJenkins(content []byte, githubEvent string, url string) (error) {
	//Send the request on to the Jenkins Url
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(content))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Github-Event", githubEvent)

	client := &http.Client{
		Timeout: 15 * time.Second,
	}

	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	//test we are getting the correct status code
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%d", response.StatusCode)
	}

	return nil
}

func sendToTopic(ctx context.Context, psclient *pubsub.Client,c []byte,  topicName string) error {
	//publish the message
	m := &pubsub.Message{
		Data: []byte(c),
	}
	topic = psclient.Topic(topicName)
	id, err := topic.Publish(ctx, m).Get(ctx)
	if err != nil {
		logError(err, "Topic publish error on topic: " + topicName )
		logError(err, "Topic publish error original msg:: " + string(c))
		return err
	}
	logInfo("Message sent to topic successfully", "message-id", id)
	return nil
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
		logInfo("Retry needed", "current-attempts", strconv.Itoa(i+1))
	}
	return fmt.Errorf("after %d attempts, last error: %s", attempts, err)
}

func getVaultSecret(envars *EnvironmentVars) (secret string) {
	jwt, err := ioutil.ReadFile(K8Token)
	if err != nil {
		logFatal(err, "JWT Token")
	}
	//Set Vault Address
	vaultConfig := &vault.Config{
		Address: envars.VaultAddress,
	}

	//Set CA Cert location
	tlsConfig := &vault.TLSConfig{
		CACert: K8CACert,
	}

	tlserr := vaultConfig.ConfigureTLS(tlsConfig)
	if err != nil {
		logFatal(tlserr, "Vault TLS Config")
	}
	client, err := vault.NewClient(vaultConfig)
	if err != nil {
		logFatal(err, "Vault Client Initiation")
	}
	auth, err := client.Logical().Write("auth/kubernetes/login", map[string]interface{}{"jwt": string(jwt), "role": envars.VaultRole})
	if err != nil {
		logFatal(err, "Vault Auth")
	}
	client.SetToken(auth.Auth.ClientToken)

	secretValues, err := client.Logical().Read(envars.SecretPath)
	if err != nil {
		logFatal(err, "Vault Get Secret")
	}
	logger.Info("Vault Secret Successfully Retrieved")
	return fmt.Sprint(secretValues.Data["value"])

}


func main() {
	//Load environment vars
	envars := loadVars()
	//we call getVaultSecret here so we dont start the pod if we fail it
	secret := getVaultSecret(envars)

    //Iniate pubsub client
    ctx := context.Background()
	psclient, err := pubsub.NewClient(ctx, envars.ProjectID)
	if err != nil {
		logFatal(err, "pubsub client creation error")
	}

	// define a channel `eventChannel` it will send/receive hookEvents with a buffer, will block once that buffer is full
	eventChannel := make(chan hookEvent, 100)

	// fire up a new goroutine in the background for processing hook events
	go hookProcessor(ctx, psclient, envars, eventChannel)

	srv := &http.Server{
		Addr:         ":8090",
		Handler:      http.HandlerFunc(processRequest(secret, eventChannel)),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	logger.Info("Server Started")
	logFatal(srv.ListenAndServe(),"Server Error")
}
