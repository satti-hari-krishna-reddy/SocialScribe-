package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"social-scribe/backend/internal/models"
	repo "social-scribe/backend/internal/repositories"
	"social-scribe/backend/internal/scheduler"
	"social-scribe/backend/internal/services"
	"strings"
	"time"

	"github.com/dghubble/oauth1"
	"github.com/dghubble/oauth1/twitter"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/gorilla/mux"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"
	"math/rand"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/linkedin"
)

var twitterConfig = &oauth1.Config{}
var linkedinConfig = &oauth2.Config{}

func init() {
	err := godotenv.Load("../../.env")
	if err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}
	twitterConfig = &oauth1.Config{
		ConsumerKey:    os.Getenv("TWITTER_CONSUMER_KEY"),
		ConsumerSecret: os.Getenv("TWITTER_CONSUMER_SECRET"),
		CallbackURL:    os.Getenv("TWITTER_CALLBACK_URL"),
		Endpoint:       twitter.AuthorizeEndpoint,
	}
	linkedinConfig = &oauth2.Config{
		ClientID:     os.Getenv("LINKEDIN_CLIENT_ID"),
		ClientSecret: os.Getenv("LINKEDIN_CLIENT_SECRET"),
		RedirectURL:  os.Getenv("LINKEDIN_CALLBACK_URL"),
		Scopes:       []string{"openid", "profile", "email", "w_member_social"},
		Endpoint:     linkedin.Endpoint,
	}

	services.InitTwitterConfig(twitterConfig)

}

var taskScheduler *scheduler.Scheduler

func InitScheduler(s *scheduler.Scheduler) {
	taskScheduler = s
}

func SignupUserHandler(resp http.ResponseWriter, req *http.Request) {
	if req.Body == nil {
		http.Error(resp, `{"error": "Failed to parse credentials: body is empty"}`, http.StatusBadRequest)
		return
	}
	user := models.User{}

	err := json.NewDecoder(req.Body).Decode(&user)
	if err != nil {
		http.Error(resp, `{"error": "Bad request: unable to decode JSON"}`, http.StatusBadRequest)
		return
	}

	user.UserName = strings.TrimSpace(user.UserName)
	user.UserName = strings.Join(strings.Fields(strings.ToLower(user.UserName)), "")
	user.PassWord = strings.TrimSpace(user.PassWord)

	if len(user.UserName) < 4 || len(user.UserName) > 64 {
		http.Error(resp, `{"error": "The username should contain a minimum of 4 and maximum of 64 characters"}`, http.StatusBadRequest)
		return
	}
	if len(user.PassWord) < 8 || len(user.PassWord) > 128 {
		http.Error(resp, `{"error": "The password should contain a minimum of 8 and maximum of 128 characters"}`, http.StatusBadRequest)
		return
	}

	existingUser, err := repo.GetUserByName(user.UserName)
	if err != nil {
		http.Error(resp, `{"error": "Internal server error"}`, http.StatusInternalServerError)
		log.Printf("[ERROR] Error checking existing user: %v", err)
		return
	}
	if existingUser != nil {
		http.Error(resp, `{"message" : "Username already taken"}`, http.StatusConflict)
		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(user.PassWord), bcrypt.DefaultCost)
	if err != nil {
		http.Error(resp, `{"error": "Internal server error"}`, http.StatusInternalServerError)
		log.Printf("[ERROR] Error hashing password for user '%s': %v", user.UserName, err)
		return
	}

	user.Verified = false
	user.LinkedinVerified = false
	user.EmailVerified = false
	user.HashnodeVerified = false
	user.XVerified = false
	user.PassWord = string(hashedPassword)

	userId, err := repo.InsertUser(user)
	if err != nil {
		log.Printf("[ERROR] Unable to create user %v: %v", user.UserName, err)
		http.Error(resp, `{"error": "Failed to create user"}`, http.StatusInternalServerError)
		return
	}

	sessionToken := uuid.New().String()
	expiration := time.Now().Add(24 * time.Hour)
	err = repo.SetCache(sessionToken, userId, 24*time.Hour)
	if err != nil {
		http.Error(resp, `{"error": "Failed to create session"}`, http.StatusInternalServerError)
		return
	}

	http.SetCookie(resp, &http.Cookie{
		Name:     "session_token",
		Value:    sessionToken,
		HttpOnly: true,
		Path:     "/",
		Secure:   false,
		Expires:  expiration,
	})

	user.PassWord = ""
	responseJson, err := json.Marshal(user)
	if err != nil {
		resp.WriteHeader(http.StatusInternalServerError)
		resp.Write([]byte(`{"success": false, "reason": "Failed unpacking user"}`))
		return
	}

	log.Printf("[INFO] User '%s' successfully registered with ID: %s", user.UserName, userId)

	resp.WriteHeader(http.StatusCreated)
	resp.Header().Set("Content-Type", "application/json")
	resp.Write([]byte(responseJson))
}

func LoginUserHandler(resp http.ResponseWriter, req *http.Request) {

	if req.Body == nil {
		http.Error(resp, `{"error": "Failed to parse login credentials: body is empty"}`, http.StatusBadRequest)
		return
	}
	data := models.LoginStruct{}

	err := json.NewDecoder(req.Body).Decode(&data)
	if err != nil {
		http.Error(resp, `{"error": "Bad request: unable to decode JSON"}`, http.StatusBadRequest)
		return
	}

	data.Username = strings.ToLower(strings.TrimSpace(data.Username))
	if len(data.Username) < 4 || len(data.Username) > 64 {
		http.Error(resp, `{"error": "Username is should in range of minimum 4 to maximum 64 characters}`, http.StatusBadGateway)
	}
	if len(data.Password) > 128 {
		http.Error(resp, `{"error" : "password is too long, the maximum allowed length is 128 chars"}`, http.StatusBadGateway)
	}
	user, err := repo.GetUserByName(data.Username)
	if user == nil {
		http.Error(resp, `{"success": false, "reason": "Username and/or password is incorrect"}`, http.StatusBadRequest)
		return
	}
	if err != nil {
		log.Printf("[ERROR] Failed to get user for the username %s and the error is %s", data.Username, err)
		http.Error(resp, `{"error" : "Internal server error"}`, http.StatusInternalServerError)
		return
	}
	err = bcrypt.CompareHashAndPassword([]byte(user.PassWord), []byte(data.Password))
	if err != nil {
		http.Error(resp, `{"success": false, "reason": "Username and/or password is incorrect"}`, http.StatusBadRequest)
		return
	}

	sessionToken := uuid.New().String()
	expiration := time.Now().Add(24 * time.Hour)
	err = repo.SetCache(sessionToken, user.Id, 24*time.Hour)
	if err != nil {
		http.Error(resp, `{"error": "Failed to create session"}`, http.StatusInternalServerError)
		return
	}

	http.SetCookie(resp, &http.Cookie{
		Name:     "session_token",
		Value:    sessionToken,
		HttpOnly: true,
		Path:     "/",
		Secure:   false,
		Expires:  expiration,
	})

	user.PassWord = ""
	responseJson, err := json.Marshal(user)
	if err != nil {
		resp.WriteHeader(401)
		resp.Write([]byte(`{"success": false, "reason": "Failed unpacking user"}`))
		return
	}

	resp.WriteHeader(http.StatusAccepted)
	resp.Header().Set("Content-Type", "application/json")
	resp.Write([]byte(responseJson))
}

func GetUserInfoHandler(resp http.ResponseWriter, req *http.Request) {
	userId, err := ValidateLogin(req)
	if err != nil {
		http.Error(resp, "Unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] Failed to find user for the id: %s and error is %s", userId, err)
		http.Error(resp, `{"error": ""}`, http.StatusInternalServerError)
		return
	}
	if user == nil {
		http.Error(resp, `{"error": "user id is not valid"}`, http.StatusNotFound)
		return
	}
	user.PassWord = ""
	user.HashnodePAT = ""
	user.LinkedInOauthKey = ""
	user.XOAuthToken = ""
	user.XOAuthSecret = ""
	responseJson, err := json.Marshal(user)
	if err != nil {
		resp.WriteHeader(401)
		resp.Write([]byte(`{"success": false, "reason": "Failed unpacking"}`))
		return
	}

	resp.WriteHeader(http.StatusOK)
	resp.Write(responseJson)
}

func GetUserNotificationsHandler(resp http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	userId := vars["id"]
	if len(userId) == 0 {
		http.Error(resp, `{"error": "cant able parse id field, reason is missing id field in the request"}`, http.StatusBadRequest)
		return
	}
	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] Failed to find user for the id: %s and error is %s", userId, err)
		http.Error(resp, `{"error": ""}`, http.StatusInternalServerError)
		return
	}
	if user == nil {
		http.Error(resp, `{"error": "user id is not valid"}`, http.StatusNotFound)
		return
	}
	respone := map[string]interface{}{
		"notifications": user.Notifications,
	}
	responseJson, err := json.Marshal(respone)
	if err != nil {
		resp.WriteHeader(401)
		resp.Write([]byte(`{"success": false, "reason": "Failed unpacking"}`))
		return
	}

	resp.WriteHeader(200)
	resp.Write(responseJson)

}

func GetUserSharedBlogsHandler(resp http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	userId := vars["id"]
	if len(userId) == 0 {
		resp.WriteHeader(401)
		resp.Write([]byte(`{"success" : false, "reason" : "user id not found in the request}`))
		return
	}
	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] Failed to find user for the id: %s and error is %s", userId, err)
		resp.WriteHeader(500)
		resp.Write([]byte(`{"success" : false}`))
		return
	}
	if user == nil {
		resp.WriteHeader(401)
		resp.Write([]byte(`{"success" : false, "reason" : "user id is invalid"}`))
		return
	}
	response := map[string]interface{}{
		"shared_blogs": user.SharedBlogs,
	}
	responseJson, err := json.Marshal(response)
	if err != nil {
		resp.WriteHeader(401)
		resp.Write([]byte(`{"sucess" : false, "reason" : "Failed unpacking}`))
		return
	}
	resp.WriteHeader(200)
	resp.Write(responseJson)

}

func GetUserScheduledBlogsHandler(resp http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	userId := vars["id"]
	if len(userId) == 0 {
		resp.WriteHeader(401)
		resp.Write([]byte(`"success" : false, "reason" : "user id not provided`))
		return

	}
	user, err := repo.GetUserById(userId)
	if user == nil {
		resp.WriteHeader(401)
		resp.Write([]byte(`"success" : false, "reason" : "user id is invalid`))
		return
	}
	if err != nil {
		log.Printf("[ERROR] Failed to find user for the id: %s and error is %s", userId, err)
		resp.WriteHeader(500)
		resp.Write([]byte(`{"success" : "false"}`))
		return
	}
	response := map[string]interface{}{
		"scheduled_blogs": user.ScheduledBlogs,
	}
	responseJson, err := json.Marshal(response)
	if err != nil {
		resp.WriteHeader(500)
		resp.Write([]byte(`{"success" : false}`))
		return
	}
	resp.WriteHeader(200)
	resp.Write(responseJson)
}

func ClearUserNotificationsHandler(resp http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	userId := vars["id"]
	if len(userId) == 0 {
		resp.WriteHeader(401)
		resp.Write([]byte(`"success" : false, "reason" : "missing user id in the request"`))
		return
	}
	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] failed to get user for the id: %s and the error is %s", userId, err)
		resp.WriteHeader(500)
		resp.Write([]byte(`{"success" : false}`))
		return
	}
	if user == nil {
		resp.WriteHeader(401)
		resp.Write([]byte(`"success" : false, "reason" : "invalid user id"`))
		return

	}
	user.Notifications = []string{}
	err = repo.UpdateUser(userId, user)
	if err != nil {
		log.Printf("[ERROR] failed to update user with id: %s", userId)
		resp.WriteHeader(500)
		resp.Write([]byte(`{"success" : false, "reason" : "}`))
		return
	}
	resp.WriteHeader(200)
	resp.Write([]byte(`{"success" : true, "message" : "notifications cleared sucessfully"}`))
}

func ScheduleUserBlogHandler(resp http.ResponseWriter, req *http.Request) {
	var blogData models.ScheduledBlogData
	decoder := json.NewDecoder(req.Body)
	defer req.Body.Close()

	if err := decoder.Decode(&blogData); err != nil {
		http.Error(resp, "Bad request, failed to parse JSON", http.StatusBadRequest)
		return
	}

	if len(blogData.UserID) == 0 {
		resp.WriteHeader(http.StatusBadRequest)
		resp.Write([]byte(`{"success" : false, "reason" : "no user id found"}`))
		return
	}

	user, err := repo.GetUserById(blogData.UserID)
	if err != nil {
		resp.WriteHeader(http.StatusInternalServerError)
		resp.Write([]byte(`{"success" : false}`))
		return
	}

	if user == nil {
		resp.WriteHeader(http.StatusBadRequest)
		resp.Write([]byte(`{"success" : false, "reason" : "user id is not valid"}`))
		return
	}

	if blogData.ScheduledBlog.ScheduledTime.IsZero() {
		resp.WriteHeader(http.StatusBadRequest)
		resp.Write([]byte(`{"success" : false, "reason" : "scheduled time is missing"}`))
		return
	}

	_, err = time.Parse(time.RFC3339, blogData.ScheduledBlog.ScheduledTime.Format(time.RFC3339))
	if err != nil {
		resp.WriteHeader(http.StatusBadRequest)
		resp.Write([]byte(`{"success" : false, "reason" : "invalid scheduled time format, must be RFC3339"}`))
		return
	}

	if err := blogData.ScheduledBlog.Validate(); err != nil {
		http.Error(resp, err.Error(), http.StatusBadRequest)
		return
	}

	durableFunctionURL := "https://<your-function-app>.azurewebsites.net/api/orchestrator"
	reqBody, _ := json.Marshal(blogData)

	durableResp, err := http.Post(durableFunctionURL, "application/json", bytes.NewBuffer(reqBody))
	if err != nil || durableResp.StatusCode != http.StatusOK {
		log.Printf("[DEBUG] Failed to create durable function, reason: %s", err)
		resp.WriteHeader(http.StatusInternalServerError)
		resp.Write([]byte(`{"success": false, "reason": "failed to create a cloud function"}`))
		return
	}

	var instanceID string
	if err := json.NewDecoder(durableResp.Body).Decode(&instanceID); err != nil {
		resp.WriteHeader(http.StatusInternalServerError)
		resp.Write([]byte(`{"success": false}`))
		return
	}

	resp.WriteHeader(http.StatusOK)
	resp.Write([]byte("Blog scheduled validated"))
}

func GetUserBlogsHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := ValidateLogin(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] Failed to get user for id: %s - %v", userId, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if user == nil {
		log.Printf("[ERROR] User with id: %s not found", userId)
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	category := strings.ToLower(r.URL.Query().Get("category"))
	if category == "" {
		category = "all"
	} else if category != "all" && category != "scheduled" && category != "shared" {
		http.Error(w, "Invalid category", http.StatusBadRequest)
		return
	}

	var responseBytes []byte
	var jsonErr error

	switch category {
	case "scheduled":
		responseBytes, jsonErr = json.Marshal(user.ScheduledBlogs)
	case "shared":
		responseBytes, jsonErr = json.Marshal(user.SharedBlogs)
	default:
		// Handle "all" case with GraphQL
		endpoint := "https://gql.hashnode.com"
		query := models.GraphQLQuery{
			Query: fmt.Sprintf(`
                query Publication {
                    publication(host: "%s") {
                        posts(first: 0) {
                            edges {
                                node {
                                    title
                                    url
                                    id
                                    coverImage { url }
                                    author { name }
                                    readTimeInMinutes
                                }
                            }
                        }
                    }
                }`, user.HashnodeBlog),
		}

		queryBytes, err := json.Marshal(query)
		if err != nil {
			log.Printf("[ERROR] Failed to marshal query: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		headers := map[string]string{"Content-Type": "application/json"}
		gqlResponse, err := services.MakePostRequest(endpoint, queryBytes, headers)
		if err != nil {
			log.Printf("[ERROR] Failed to make request: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		var gqlData models.GraphQLResponse
		if err := json.Unmarshal(gqlResponse, &gqlData); err != nil {
			log.Printf("[ERROR] Failed to unmarshal response: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		var posts []models.PostNode
		for _, edge := range gqlData.Data.Publication.Posts.Edges {
			posts = append(posts, edge.Node)
		}
		responseBytes, jsonErr = json.Marshal(posts)
	}

	// Handle JSON marshaling errors
	if jsonErr != nil {
		log.Printf("[ERROR] Failed to marshal response: %v", jsonErr)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf(`{"success": true, "blogs": %s}`, string(responseBytes))))
}

// func makePostRequest(url string, body []byte, headers map[string]string) ([]byte, error) {
// 	request, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to create HTTP request: %v", err)
// 	}

// 	for key, value := range headers {
// 		request.Header.Set(key, value)
// 	}

// 	client := &http.Client{}
// 	response, err := client.Do(request)
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to execute HTTP request: %v", err)
// 	}
// 	defer response.Body.Close()

// 	if response.StatusCode != http.StatusOK {
// 		body, _ := ioutil.ReadAll(response.Body)
// 		return nil, fmt.Errorf("GraphQL query failed with status code %d: %s", response.StatusCode, string(body))
// 	}

// 	return ioutil.ReadAll(response.Body)
// }

func ConnectXhandler(w http.ResponseWriter, r *http.Request) {
	userId, err := ValidateLogin(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] Failed to get user for the id: %s and error is %s", userId, err)
		return
	}
	if user == nil {
		log.Printf("[ERROR] User with id: %s not found", userId)
		return
	}

	requestToken, requestSecret, err := twitterConfig.RequestToken()
	if err != nil {
		fmt.Printf("error: %v", err)
		http.Error(w, "Failed to get request token", http.StatusInternalServerError)
		return
	}
	user.XOAuthToken = requestToken
	user.XOAuthSecret = requestSecret
	err = repo.UpdateUser(userId, user)
	if err != nil {
		log.Printf("[ERROR] Failed to update user with id: %s and error is %s", userId, err)
		return
	}

	authorizationURL, err := twitterConfig.AuthorizationURL(requestToken)
	if err != nil {
		http.Error(w, "Failed to get authorization URL", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, authorizationURL.String(), http.StatusFound)
}

func XcallbackHandler(w http.ResponseWriter, r *http.Request) {

	userID, err := ValidateLogin(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := repo.GetUserById(userID)
	if err != nil {
		http.Error(w, "Failed to get user", http.StatusInternalServerError)
		log.Printf("[ERROR] Failed to get user for the id: %s and error is %s", userID, err)

		return
	}
	if user == nil {
		http.Error(w, "User not found", http.StatusNotFound)
		log.Printf("[ERROR] User with id: %s not found", userID)
		return
	}

	token := user.XOAuthToken
	secret := user.XOAuthSecret

	requestTokenData := &oauth1.Token{Token: token, TokenSecret: secret}
	verifier := r.URL.Query().Get("oauth_verifier")
	if verifier == "" {
		log.Printf("[ERROR] Missing OAuth verifier for user with id: %s", userID)
		http.Error(w, "Missing OAuth verifier", http.StatusBadRequest)
		return
	}
	accessToken, accessSecret, err := twitterConfig.AccessToken(requestTokenData.Token, requestTokenData.TokenSecret, verifier)
	if err != nil {
		log.Printf("[ERROR] Failed to get access token for user with id: %s and error is %s", userID, err)
		http.Error(w, "Failed to get access token", http.StatusInternalServerError)
		return
	}
	user.XOAuthToken = accessToken
	user.XOAuthSecret = accessSecret
	user.XVerified = true
	if (user.XVerified || user.LinkedinVerified) && user.HashnodeVerified {
		user.Verified = true
	} else {
		user.Verified = false
	}
	err = repo.UpdateUser(userID, user)
	if err != nil {
		http.Error(w, "Failed to update user", http.StatusInternalServerError)
		log.Printf("[ERROR] Failed to update user with id: %s and error is %s", userID, err)
		return
	}

	log.Printf("[INFO] User with ID %s connected to X(twitter) Successfully", user.Id)
	http.Redirect(w, r, "http://localhost:5173/verification", http.StatusSeeOther)
}

// func PostTweetHandler(message string, blogId string, userToken *oauth1.Token) error {

// 	client := twitterConfig.Client(oauth1.NoContext, userToken)

// 	tweetURL := "https://api.twitter.com/1.1/statuses/update.json"
// 	resp, err := client.PostForm(tweetURL, map[string][]string{"status": {message}})
// 	if err != nil {
// 		log.Printf("[ERROR] Failed to post tweet for the blog id : %s and the error is %s", blogId, err)
// 		return err
// 	}
// 	defer resp.Body.Close()

// 	if resp.StatusCode != http.StatusOK {
// 		return errors.New("Failed to post tweet: " + resp.Status)
// 	}

// 	log.Printf("[INFO] Blog with ID %s shared on X(twitter) Successfully", blogId)
// 	return nil
// }

func ConnectLinkedInHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := ValidateLogin(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] Failed to get user for the id: %s and error is %s", userId, err)
		return
	}
	if user == nil {
		log.Printf("[ERROR] User with id: %s not found", userId)
		return
	}
	state := uuid.New().String()
	err = repo.SetCache(state, userId, 10*time.Minute)
	if err != nil {
		log.Printf("[ERROR] Failed to store state in cache: %v", err)
		http.Error(w, "Failed to store state in cache", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		HttpOnly: true,
		Path:     "/",
		Secure:   false,
	})

	authURL := linkedinConfig.AuthCodeURL(state)
	http.Redirect(w, r, authURL, http.StatusFound)
}

func LinkedCallbackHandler(w http.ResponseWriter, r *http.Request) {
	queryState := r.URL.Query().Get("state")
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil || stateCookie.Value != queryState {
		log.Printf("[ERROR] Invalid state parameter")
		http.Error(w, "Invalid state parameter", http.StatusForbidden)
		return
	}
	userId, exists := repo.GetCache(stateCookie.Value)
	if !exists {
		log.Printf("[ERROR] Invalid state parameter")
		http.Error(w, "Invalid state parameter", http.StatusForbidden)
		return
	}
	err = repo.DeleteCache(stateCookie.Value)
	if err != nil {
		log.Printf("[WARN] Failed to delete state from cache for the user id: %s and error is %s", userId, err)
	}

	user, err := repo.GetUserById(userId.(string))
	if err != nil {
		log.Printf("[ERROR] Failed to get user for the id: %s and error is %s", userId, err)
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if user == nil {
		log.Printf("[ERROR] User with id: %s not found", userId)
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		log.Printf("[ERROR] Missing authorization code")
		http.Error(w, "Missing authorization code", http.StatusBadRequest)
		return
	}

	ctx := context.Background()
	token, err := linkedinConfig.Exchange(ctx, code)
	if err != nil {
		http.Error(w, "Failed to exchange token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	user.LinkedInOauthKey = token.AccessToken
	user.LinkedinVerified = true
	if (user.XVerified || user.LinkedinVerified) && user.HashnodeVerified {
		user.Verified = true
	} else {
		user.Verified = false
	}
	err = repo.UpdateUser(userId.(string), user)
	if err != nil {
		log.Printf("[ERROR] Failed to update user with id: %s and error is %s", userId, err)
		http.Error(w, "Failed to update user", http.StatusInternalServerError)
		return
	}
	log.Printf("[INFO] User with ID %s connected to LinkedIn Successfully", user.Id)

	// Redirect the user back to the frontend
	http.Redirect(w, r, "http://localhost:5173/verification", http.StatusSeeOther)
}


func ValidateLogin(req *http.Request) (string, error) {
	cookie, err := req.Cookie("session_token")
	if err != nil {
		return "", fmt.Errorf("missing session token")
	}

	sessionData, exists := repo.GetCache(cookie.Value)
	if !exists {
		return "", fmt.Errorf("invalid or expired session")
	}

	session, ok := sessionData.(models.CacheItem)
	if !ok {
		return "", fmt.Errorf("invalid session data format")
	}

	if session.ExpiresAt.Before(time.Now()) {
		return "", fmt.Errorf("session expired")
	}

	// session.Value is actually a primitive.ObjectID, convert it to string.
	oid, ok := session.Value.(primitive.ObjectID)
	if !ok {
		return "", fmt.Errorf("invalid session user id format")
	}
	return oid.Hex(), nil
}

func VerifyHashnodeHandler(w http.ResponseWriter, r *http.Request) {
	endpoint := "https://gql.hashnode.com"
	userId, err := ValidateLogin(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] Failed to get user for the id: %s and error is %s", userId, err)
		return
	}
	if user == nil {
		log.Printf("[ERROR] User with id: %s not found", userId)
		return
	}

	var hashnodeKey models.HashnodeKey
	err = json.NewDecoder(r.Body).Decode(&hashnodeKey)
	if err != nil {
		http.Error(w, "Failed to parse JSON", http.StatusBadRequest)
		return
	}
	if hashnodeKey.Key == "" {
		http.Error(w, "Missing Hashnode API key", http.StatusBadRequest)
		return
	}

	query := `{"query":"query Me { me { publications(first:1) { edges { node { url id } } } } }"}`

	req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer([]byte(query)))
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", hashnodeKey.Key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "Failed to make request", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, "Invalid Hashnode API key", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Failed to read response", http.StatusInternalServerError)
		return
	}

	var response struct {
		Data struct {
			Me struct {
				Publications struct {
					Edges []struct {
						Node struct {
							URL string `json:"url"`
							ID  string `json:"id"`
						} `json:"node"`
					} `json:"edges"`
				} `json:"publications"`
			} `json:"me"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		http.Error(w, "Failed to parse response JSON", http.StatusInternalServerError)
		return
	}

	// Check if we have at least one publication
	if len(response.Data.Me.Publications.Edges) == 0 {
		http.Error(w, "No publications found", http.StatusNotFound)
		return
	}

	// Extract `url` and `id`
	node := response.Data.Me.Publications.Edges[0].Node
	url := strings.ReplaceAll(node.URL, "https://", "")
	id := node.ID

	user.HashnodePAT = hashnodeKey.Key
	user.HashnodeVerified = true
	user.HashnodeBlog = url
	if (user.XVerified || user.LinkedinVerified) && user.HashnodeVerified {
		user.Verified = true
	} else {
		user.Verified = false
	}
	err = repo.UpdateUser(userId, user)
	if err != nil {
		log.Printf("[ERROR] Failed to update user with id: %s and error is %s", userId, err)
		return
	}
	fmt.Printf(`{"success": true, "url": "%s", "id": "%s"}`, url, id)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success": true}`))
}

func ShareBlogHandler(w http.ResponseWriter, req *http.Request) {
	userId, err := ValidateLogin(req)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] Failed to get user for the id: %s and error is %s", userId, err)
		return
	}
	if user == nil {
		log.Printf("[ERROR] User with id: %s not found", userId)
		return
	}
	if !user.Verified {
		http.Error(w, "User is not verified", http.StatusForbidden)
		return
	}

	var requestBody struct {
		Id        string   `json:"id"`
		Platforms []string `json:"platforms"`
	}
	if err := json.NewDecoder(req.Body).Decode(&requestBody); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	blogId := requestBody.Id
	if len(blogId) == 0 {
		w.WriteHeader(401)
		w.Write([]byte(`{"success": false, "reason": "missing blog id in the request"}`))
		return
	}

	err = services.ProcessSharedBlog(user, blogId, requestBody.Platforms)
	if err != nil {
		log.Printf("[ERROR] Failed to share blog: %v", err)
		http.Error(w, "Failed to share blog", http.StatusInternalServerError)
		return
	}
	log.Printf("[INFO] Blog with ID %s shared successfully by user with ID %s", blogId, userId)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success": true}`))
}

func ScheduleBlogHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := ValidateLogin(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] Failed to get user for the id: %s and error is %s", userId, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if user == nil {
		log.Printf("[ERROR] User with id: %s not found", userId)
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if !user.Verified {
		log.Printf("[ERROR] User with id: %s is not verified", userId)
		http.Error(w, "User is not verified", http.StatusForbidden)
		return
	}
	var blogData models.ScheduledBlogData
	err = json.NewDecoder(r.Body).Decode(&blogData)
	if err != nil {
		http.Error(w, "Failed to parse JSON", http.StatusBadRequest)
		return
	}
	blogData.UserID = userId
	err = blogData.ScheduledBlog.Validate()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	//check if the user has already scheduled the blog
	for i := range user.ScheduledBlogs {
		if user.ScheduledBlogs[i].Id == blogData.ScheduledBlog.Id {
			http.Error(w, "Blog already scheduled", http.StatusBadRequest)
			return
		}
	}

	err = taskScheduler.AddTask(blogData)
	if err != nil {
		http.Error(w, "Failed to store scheduled task", http.StatusInternalServerError)
		return
	}

	user.ScheduledBlogs = append(user.ScheduledBlogs, blogData.ScheduledBlog)
	err = repo.UpdateUser(userId, user)
	if err != nil {
		log.Printf("[ERROR] Failed to update user with id: %s and error is %s", userId, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	log.Printf("[INFO] Blog with ID %s scheduled successfully by user with ID %s", blogData.ScheduledBlog.Id, userId)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success": true}`))

}

func CancelScheduledBlogHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := ValidateLogin(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] Failed to get user for the id: %s and error is %s", userId, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if user == nil {
		log.Printf("[ERROR] User with id: %s not found", userId)
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	if !user.Verified {
		log.Printf("[ERROR] User with id: %s is not verified", userId)
		http.Error(w, "User is not verified", http.StatusForbidden)
		return
	}
	var requestBody struct {
		Id string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	blogId := requestBody.Id
	if len(blogId) == 0 {
		http.Error(w, "Missing blog id", http.StatusBadRequest)
		return
	}
	var updatedScheduledBlogs []models.ScheduledBlog
	for _, blog := range user.ScheduledBlogs {
		if blog.Id == blogId {
			continue
		}
		updatedScheduledBlogs = append(updatedScheduledBlogs, blog)
	}
	user.ScheduledBlogs = updatedScheduledBlogs
	err = repo.UpdateUser(userId, user)
	if err != nil {

		log.Printf("[ERROR] Failed to update user with id: %s and error is %s", userId, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	err = taskScheduler.RemoveTask(blogId)
	if err != nil {
		log.Printf("[ERROR] Failed to remove scheduled task with id: %s and error is %s", blogId, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	log.Printf("[INFO] Scheduled blog with ID %s cancelled successfully by user with ID %s", blogId, userId)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success": true}`))
}

func VerifyEmailHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := ValidateLogin(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] Failed to get user for the id: %s and error is %s", userId, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if user == nil {
		log.Printf("[ERROR] User with id: %s not found", userId)
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	var requestBody struct {
		Otp string `json:"otp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	// get the otp from the cache
	cacheKey := fmt.Sprintf("email_otp_%s", userId)
	cachedOtp, exists := repo.GetCache(cacheKey)
	if !exists {
		http.Error(w, "OTP expired", http.StatusBadRequest)
		return
	}
	if cachedOtp != requestBody.Otp {
		http.Error(w, "Invalid OTP", http.StatusBadRequest)
		return
	}
	user.EmailVerified = true
	if (user.XVerified || user.LinkedinVerified) && user.HashnodeVerified  && user.EmailVerified {
		user.Verified = true
	} else {
		user.Verified = false
	}
	err = repo.UpdateUser(userId, user)
	if err != nil {
		log.Printf("[ERROR] Failed to update user with id: %s and error is %s", userId, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	log.Printf("[INFO] User with ID %s verified email successfully", userId)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success": true}`))
}

func ResetEmailOtpHandler(w http.ResponseWriter, r *http.Request) {
	userId, err := ValidateLogin(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := repo.GetUserById(userId)
	if err != nil {
		log.Printf("[ERROR] Failed to get user for the id: %s and error is %s", userId, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if user == nil {
		log.Printf("[ERROR] User with id: %s not found", userId)
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}
	// delete the old otp
	cacheKey := fmt.Sprintf("email_otp_%s", userId)
	err = repo.DeleteCache(cacheKey)
	if err != nil {
		log.Printf("[ERROR] Failed to delete old OTP for the user id: %s and error is %s", userId, err)
	}
	// generate new otp
	otp := fmt.Sprintf("%06d", rand.Intn(1000000))
	err = repo.SetCache(cacheKey, otp, 5*time.Minute)
	if err != nil {
		log.Printf("[ERROR] Failed to store new OTP for the user id: %s and error is %s", userId, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	log.Printf("[INFO] New OTP generated for the user with ID %s", userId)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success": true}`))
}

// func VerifyHashnodeHandler(w http.ResponseWriter, r *http.Request) {
// 	endpoint := "https://gql.hashnode.com"
// 	userId, err := ValidateLogin(r)
// 	if err != nil {
// 		http.Error(w, "Unauthorized", http.StatusUnauthorized)
// 		return
// 	}
// 	user, err := repo.GetUserById(userId)
// 	if err != nil {
// 		log.Printf("[ERROR] Failed to get user for the id: %s and error is %s", userId, err)
// 		return
// 	}
// 	if user == nil {
// 		log.Printf("[ERROR] User with id: %s not found", userId)
// 		return
// 	}

// 	var hashnodeKey models.HashnodeKey
// 	err = json.NewDecoder(r.Body).Decode(&hashnodeKey)
// 	if err != nil {
// 		http.Error(w, "Failed to parse JSON", http.StatusBadRequest)
// 		return
// 	}
// 	if hashnodeKey.Key == "" {
// 		http.Error(w, "Missing Hashnode API key", http.StatusBadRequest)
// 		return
// 	}

// 	query := `{"query":"query Me { me { publications(first:1) { edges { node { url id } } } } }"}`

// 	req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer([]byte(query)))
// 	if err != nil {
// 		http.Error(w, "Failed to create request", http.StatusInternalServerError)
// 		return
// 	}
// 	req.Header.Set("Content-Type", "application/json")
// 	req.Header.Set("Authorization", hashnodeKey.Key)

// 	resp, err := http.DefaultClient.Do(req)
// 	if err != nil {
// 		http.Error(w, "Failed to make request", http.StatusInternalServerError)
// 		return
// 	}
// 	defer resp.Body.Close()

// 	if resp.StatusCode != http.StatusOK {
// 		http.Error(w, "Invalid Hashnode API key", http.StatusUnauthorized)
// 		return
// 	}

// 	body, err := io.ReadAll(resp.Body)
// 	if err != nil {
// 		http.Error(w, "Failed to read response", http.StatusInternalServerError)
// 		return
// 	}

// 	var response struct {
// 		Data struct {
// 			Me struct {
// 				Publications struct {
// 					Edges []struct {
// 						Node struct {
// 							URL string `json:"url"`
// 							ID  string `json:"id"`
// 						} `json:"node"`
// 					} `json:"edges"`
// 				} `json:"publications"`
// 			} `json:"me"`
// 		} `json:"data"`
// 	}
// 	if err := json.Unmarshal(body, &response); err != nil {
// 		http.Error(w, "Failed to parse response JSON", http.StatusInternalServerError)
// 		return
// 	}

// 	// Check if we have at least one publication
// 	if len(response.Data.Me.Publications.Edges) == 0 {
// 		http.Error(w, "No publications found", http.StatusNotFound)
// 		return
// 	}

// 	// Extract `url` and `id`
// 	node := response.Data.Me.Publications.Edges[0].Node
// 	url :=  strings.ReplaceAll(node.URL, "https://", "")
// 	id := node.ID

// 	// create a webhook for the user
//     webhookUrl := fmt.Sprintf("https://localhost:9696/api/v1/user/webhook/%s", userId)

// 	webhookInput := fmt.Sprintf(`{
// 		"publicationId": %s,
// 		"url": %s,
// 		"events": ["POST_PUBLISHED"],
// 		"secret": %s,
// 	  }`, id, webhookUrl, userId)

// 	queryStruct := models.GraphQLQuery{
// 		Query: fmt.Sprintf(`mutation CreateWebhook(input: /"%s/") {
// 		createWebhook(input: /"%s/") {
// 		  webhook {
// 			id
// 			url
// 			events
// 			secret
// 			createdAt
// 		  }
// 		}
// 	  }`, webhookInput, webhookInput),
// 	  }
// 	queryBytes, err := json.Marshal(queryStruct)
// 	if err != nil {
// 		http.Error(w, "Failed to marshal query", http.StatusInternalServerError)
// 		return
// 	}

// 	req, err = http.NewRequest("POST", endpoint, queryBytes)
// 	if err != nil {
// 		http.Error(w, "Failed to create request", http.StatusInternalServerError)
// 		return
// 	}
// 	req.Header.Set("Content-Type", "application/json")
// 	req.Header.Set("Authorization", hashnodeKey.Key)

// 	resp, err = http.DefaultClient.Do(req)
// 	if err != nil {
// 		http.Error(w, "Failed to make request", http.StatusInternalServerError)
// 		return
// 	}
// 	defer resp.Body.Close()

// 	if resp.StatusCode != http.StatusOK {
// 		http.Error(w, "Invalid Hashnode API key", http.StatusUnauthorized)
// 		return
// 	}

// 	body, err = io.ReadAll(resp.Body)
// 	if err != nil {
// 		http.Error(w, "Failed to read response", http.StatusInternalServerError)
// 		return
// 	}

// 	// Update the user with the Hashnode API key
// 	user.HashnodePAT = hashnodeKey.Key
// 	user.HashnodeVerified = true
// 	user.HashnodeBlog = url
// 	user.WebHookUrl = webhookUrl
// 	if (user.XVerified || user.LinkedinVerified) && user.EmailVerified && user.HashnodeVerified {
// 		user.Verified = true
// 	} else {
// 		user.Verified = false
// 	}
// 	err = repo.UpdateUser(userId, user)
// 	if err != nil {
// 		log.Printf("[ERROR] Failed to update user with id: %s and error is %s", userId, err)
// 		return
// 	}
// 	w.WriteHeader(http.StatusOK)
// 	fmt.Fprintf(w, `{"success": true, "url": "%s", "id": "%s"}`, url, id)
// }
