package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"github.com/gorilla/mux"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type RatingDate time.Time

type Config struct {
	ListenAddress string
	MongoDB struct {
		Database string
		Password string
		URI string
		Username string
	}
	UserConfigs []struct {
		Username string
		Password string
	}
}

type Rating struct {
	Date RatingDate `json:"date,string"`
	Description string `json:"description"`
	Feeling string `json:"feeling"`
	Tags []string `json:"tags"`
}

type UserData struct {
	ID interface{} `bson:"_id,omitempty"`
	Username string
	Ratings []Rating
	Tags []string
}

var (
	client mongo.Client
	config = Config{ListenAddress:":80"}
	validFeelingsList = []string{"Angry", "Bored", "Great", "Good", "Normal", "Sad"}
)

func main() {
	config.LoadConfig()
	client = openConnection()

	r := mux.NewRouter()
	r.HandleFunc("/new", NewHandler).Methods("POST")
	r.HandleFunc("/rate", RateHandler).Methods("POST").Headers("Content-Type", "application/json")
	r.HandleFunc("/ratings", RatingsHandler).Methods("GET")
	r.HandleFunc("/tags", TagsHandler).Methods("GET")

	r.Use(BasicAuthMiddlewareFunc)

	log.Println("Listening on", config.ListenAddress)
	http.ListenAndServe(config.ListenAddress, r)
}

func BasicAuthMiddlewareFunc(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()

		if ok && config.UserConfigs != nil {
			for i := 0; i < len(config.UserConfigs); i++ {
				if subtle.ConstantTimeCompare([]byte(config.UserConfigs[i].Username), []byte(username)) == 1 && subtle.ConstantTimeCompare([]byte(config.UserConfigs[i].Password), []byte(password)) == 1 {
					log.Println("Access granted to", username)
					ctx := context.WithValue(r.Context(), "Username", username)
					handler.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
		}

		log.Println("Access denied")
		http.Error(w, "Forbidden", http.StatusForbidden)
	})
}

func (config *Config) LoadConfig() {
	environment := os.Getenv("ENVIRONMENT")
	if len(environment) == 0 {
		environment = "dev"
	}

	configFile, err := os.Open("config." + environment + ".json")
	if err != nil {
		panic(err)
	}
	defer configFile.Close()

	log.Println("Loading config from", configFile.Name())
	json.NewDecoder(configFile).Decode(config)
}

func NewHandler(w http.ResponseWriter, r *http.Request) {
	username := fmt.Sprintf("%v", r.Context().Value("Username"))

	userData := getUserData(username)
	if userData != nil {
		w.WriteHeader(http.StatusConflict)
		return
	}

	if saveUserData(&UserData{Username:username,Ratings:[]Rating{},Tags:[]string{}}) {
		log.Println("New user data created for", username)
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func RateHandler(w http.ResponseWriter, r *http.Request) {
	username := fmt.Sprintf("%v", r.Context().Value("Username"))
	userData := getUserData(username)
	if userData == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var rating Rating
	err := json.NewDecoder(r.Body).Decode(&rating)
	if err != nil {
		log.Println("Unable to decode JSON", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !validRating(&rating) {
		log.Println("Rating is not valid")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	updateTags(userData, rating.Tags)
	userData.Ratings = append(userData.Ratings, rating)

	if saveUserData(userData) {
		log.Println("Received rating", rating)
		w.WriteHeader(http.StatusAccepted)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func RatingsHandler(w http.ResponseWriter, r *http.Request) {
	username := fmt.Sprintf("%v", r.Context().Value("Username"))
	userData := getUserData(username)
	if userData == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	log.Println("Returning ratings", userData.Ratings)
	json.NewEncoder(w).Encode(userData.Ratings)
}

func TagsHandler(w http.ResponseWriter, r *http.Request) {
	username := fmt.Sprintf("%v", r.Context().Value("Username"))
	userData := getUserData(username)
	if userData == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	log.Println("Returning tags", userData.Tags)
	json.NewEncoder(w).Encode(userData.Tags)
}

func (rd RatingDate) MarshalBSONValue() (bsontype.Type, []byte, error) {
	dt := primitive.NewDateTimeFromTime(time.Time(rd))

	var data []byte
	data = bsoncore.AppendDateTime(data, int64(dt))

	return bsontype.DateTime, data, nil
}

func (rd *RatingDate) UnmarshalBSONValue(t bsontype.Type, raw []byte) error {
	dt, _, ok := bsoncore.ReadDateTime(raw)
	if !ok {
		err := fmt.Errorf("unable to read date time")
		log.Fatal(err)
		return err
	}

	*rd = RatingDate(primitive.DateTime(dt).Time())
	return nil
}

func (rd *RatingDate) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), "\"")
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return err
	}

	*rd = RatingDate(t)
	return nil
}

func (rd RatingDate) MarshalJSON() ([]byte, error) {
	t := time.Time(rd)
	return json.Marshal(t.Format("2006-01-02"))
}

func existingTag(tags []string, newTag string) bool {
	for _, tag := range tags {
		if tag == newTag {
			return true
		}
	}

	return false
}

func getCollection() *mongo.Collection {
	return client.Database(config.MongoDB.Database).Collection("Ratings")
}

func getUserData(username string) *UserData {
	var userData UserData

	collection := getCollection()

	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	err := collection.FindOne(ctx, bson.M{"username": username}).Decode(&userData)
	if err != nil {
		log.Println("No user data found for", username)
		return nil
	}

	log.Println("User data found for", username)
	return &userData
}

func openConnection() mongo.Client {
	client, err := mongo.NewClient(options.Client().ApplyURI(config.MongoDB.URI).SetAppName("myday-service").SetAuth(options.Credential{Username:config.MongoDB.Username, Password:config.MongoDB.Password}))
	if err != nil {
		panic(err)
	}

	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	err = client.Connect(ctx)
	if err != nil {
		panic(err)
	}

	log.Println("Opened connection to MongoDB", config.MongoDB.URI)
	return *client
}

func saveUserData(userData *UserData) bool {
	collection := getCollection()

	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)

	var err error
	if userData.ID == nil {
		_, err = collection.InsertOne(ctx, userData)
	} else {
		_, err = collection.ReplaceOne(ctx, bson.M{"_id": userData.ID}, userData)
	}

	if err != nil {
		log.Fatal(err)
		return false
	} else {
		return true
	}
}

func updateTags(userData *UserData, newTags []string) {
	for _, newTag := range newTags {
		if !existingTag(userData.Tags, newTag) {
			userData.Tags = append(userData.Tags, newTag)
		}
	}
}

func validFeeling(feeling string) bool {
	for _, validFeeling := range validFeelingsList {
		if validFeeling == feeling {
			return true
		}
	}

	return false
}

func validRating(rating *Rating) bool  {
	return rating != nil && len(rating.Feeling) > 0 && validFeeling(rating.Feeling)
}
