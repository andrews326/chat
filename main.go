package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"os"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/websocket/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"sync"
)

// Client represents a connected client.
type Client struct {
	conn   *websocket.Conn
	send   chan []byte
	userID string
}

// Message represents a chat message.
// Message represents a chat message.
type Message struct {
	Content           string             `bson:"content"`
	Timestamp         time.Time          `bson:"timestamp"`
	UserID            string             `bson:"userID"`
	MessageID         primitive.ObjectID `bson:"messageId"`
	DeleteForMe       []string           `bson:"deleteForMe"`
	DeleteForEveryOne bool               `bson:"deleteForEveryOne"`
}

// ChatRoom represents a chat room.
type ChatRoom struct {
	clients         map[*Client]bool
	broadcast       chan []byte
	register        chan *Client
	unregister      chan *Client
	history         []string
	deleteBroadcast chan string
	updateBroadcast chan string
}

func NewChatRoom() *ChatRoom {
	return &ChatRoom{
		clients:         make(map[*Client]bool),
		broadcast:       make(chan []byte),
		register:        make(chan *Client),
		unregister:      make(chan *Client),
		history:         make([]string, 0),
		deleteBroadcast: make(chan string),
		updateBroadcast: make(chan string),
	}
}

func (c *ChatRoom) Run() {
	for {
		select {
		case client := <-c.register:
			c.clients[client] = true
			fmt.Println("New client registered")

			// Send chat history to the new client
			for _, message := range c.history {
				client.send <- []byte(message)
			}

		case client := <-c.unregister:
			if _, ok := c.clients[client]; ok {
				delete(c.clients, client)
				close(client.send)
				fmt.Println("Client unregistered")
			}

		case message := <-c.broadcast:
			c.history = append(c.history, string(message))

			for client := range c.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(c.clients, client)
				}
			}

		case deletedMessageID := <-c.deleteBroadcast:
			// Broadcast deleted messages to clients
			for client := range c.clients {
				select {
				case client.send <- []byte(fmt.Sprintf("/delete|%s", deletedMessageID)):
				default:
					// If sending fails, close the connection and unregister the client
					close(client.send)
					delete(c.clients, client)
				}
			}
		}
	}
}

var chatRoom = NewChatRoom()

func (client *Client) Read() {
	defer func() {
		chatRoom.unregister <- client
		client.conn.Close()
	}()

	for {
		_, message, err := client.conn.ReadMessage()
		if err != nil {
			break
		}
		handleMessage(client, message)
	}
}

var audioMutex sync.Mutex
var audioChunks [][]byte

func handleMessage(client *Client, message []byte) {
	fmt.Println("Received message:", string(message))

	if len(message) < 1 || message[0] != '/' {
		fmt.Println("Invalid message format")
		return
	}

	parts := bytes.SplitN(message[1:], []byte{' '}, 2)
	if len(parts) < 2 {
		fmt.Println("Invalid message format")
		return
	}

	command := string(append([]byte{'/'}, parts[0]...))
	content := string(parts[1])


	switch command {
	case "/add":
		// fmt.Println("hi insert function call")
		newMessage, err := insertMessageToDB(content, client.userID)
		if err != nil {
			log.Println("Error inserting message to DB:", err)
			return
		}
		chatRoom.broadcast <- []byte(fmt.Sprintf("%s|%s|%s", newMessage.UserID, newMessage.Content, newMessage.MessageID.Hex()))

	case "/update":
		go func() {
			var parts = strings.Fields(content)
			if len(parts) < 2 {
				fmt.Println("Invalid update message format")
				return
			}

			messageID := parts[0]
			userID := parts[1]
			updatedContent := strings.Join(parts[2:], " ")

			err := UpdateMessage(messageID, userID, updatedContent)
			if err != nil {
				log.Println("err :", err)
				return
			}
		}()
		// Add a new case for broadcasting updates
	case "/broadcast_update":
		// Extract payload from the content
		var payload map[string]string
		if err := json.Unmarshal([]byte(content), &payload); err != nil {
			log.Println("Error decoding JSON payload:", err)
			return
		}

		updatedMessageID := payload["messageID"]
		updatedUserID := payload["userID"]
		updatedContent := payload["content"]

		// Notify all clients about the update
		updateMessage := fmt.Sprintf("%s|%s|%s|%s", updatedUserID, "/update", updatedMessageID, updatedContent)
		chatRoom.broadcast <- []byte(updateMessage)
		// Broadcast the updated message ID
		chatRoom.updateBroadcast <- updatedMessageID
	

	case "/delete_for_me":
		//  fmt.Println("delete function cal")
		//  fmt.Println("content is --->>",content,"client id is -- - >>",client.userID)
		err := deleteForMe(content, client.userID)
		if err != nil {
			log.Println("Error Deleting Message", err)
			return
		}

	case "/delete_for_everyone":
		fmt.Println("delete_for_everyone case call")
		// Extract payload from the content
		var payload map[string]string
		if err := json.Unmarshal([]byte(content), &payload); err != nil {
			log.Println("Error decoding JSON payload:", err)
			return
		}

		messageID := payload["messageID"]
		userID := payload["userID"]
		loggedId := payload["loggedInUserID"]
		fmt.Println("logged user id --->>>", loggedId)
		if loggedId == "" {
			return
		}

		// fmt.Println("Message ID is --->>", messageID, "User ID is --->>", userID)
		err := deleteForEveryOne(messageID, userID, loggedId)
		if err != nil {
			log.Println("Error Deleting Message", err)
			return
		}

	case "/history":
		history, err := getAllMessages(client.userID)
		if err != nil {
			log.Println("Error retrieving chat history:", err)
			return
		}
		for _, msg := range history {
			client.send <- []byte(msg)
		}
	
	// Add other cases as needed
	default:
		chatRoom.broadcast <- message
	}
}

func (client *Client) Write() {
	defer func() {
		chatRoom.unregister <- client
		client.conn.Close()
	}()

	for {
		select {
		case message, ok := <-client.send:
			if !ok {
				client.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			err := client.conn.WriteMessage(websocket.TextMessage, message)
			if err != nil {
				return
			}
		}
	}
}

func (cr *ChatRoom) ServeWebSocket(ctx *fiber.Ctx, userId string) error {
	fmt.Println("ServeWebSocket called")
	fmt.Println("ctx:", ctx)
	fmt.Println("ctx.Path():", ctx.Path())

	conn := websocket.New(func(c *websocket.Conn) {
		fmt.Println("WebSocket connection opened")

		client := &Client{
			conn:   c,
			send:   make(chan []byte),
			userID: userId,
		}

		cr.register <- client

		go client.Write()
		client.Read()
	})

	return conn(ctx)
}

func parseUserID(path string) string {
	// Remove the '/ws' prefix
	path = strings.TrimPrefix(path, "/ws")
	// Extract the user ID (assuming it's the remaining part of the path)
	return strings.TrimPrefix(path, "/")
}

func initMongoDB() (*mongo.Client, error) {
	clientOptions := options.Client().ApplyURI("mongodb://localhost:27017")
	client, err := mongo.Connect(context.Background(), clientOptions)
	if err != nil {
		return nil, err
	}

	err = client.Ping(context.Background(), nil)
	if err != nil {
		return nil, err
	}

	fmt.Println("Connected to MongoDB")

	return client, nil
}

func insertMessageToDB(message string, userID string) (Message, error) {
	client, err := initMongoDB()
	if err != nil {
		log.Println("Error connecting to MongoDB:", err)
		return Message{}, err
	}
	defer client.Disconnect(context.Background())

	collection := client.Database("chat").Collection("messages")
	newMessageID := primitive.NewObjectID()
	// indianTimeZone,err  := time.LoadLocation("Asia/Kolkata")
	// if err != nil{
	// 	return Message{},err
	// }
	// indianTime := time.Now().In(indianTimeZone)
	// formatedTime := indianTime.Format("2006-01-02 Monday 15:04:05")
	insertResult, err := collection.InsertOne(context.Background(), bson.M{
		"content":           message,
		"timestamp":         time.Now(),
		"userID":            userID,
		"messageId":         newMessageID,
		"deleteForEveryOne": false,
		// "deleteForMe":       "",
	})
	if err != nil {
		log.Println("Error inserting message:", err)
		return Message{}, err
	}

	log.Printf("Inserted message with ID: %v\n", insertResult.InsertedID)
	if err != nil {
		return Message{}, err
	}

	newMessage := Message{
		Content:   message,
		Timestamp: time.Now(),
		UserID:    userID,
		MessageID: newMessageID,
	}
	return newMessage, nil
}

func getAllMessages(userID string) ([]string, error) {
	client, err := initMongoDB()
	if err != nil {
		return nil, err
	}
	fmt.Println("user id is ->>", userID)
	defer client.Disconnect(context.Background())

	collection := client.Database("chat").Collection("messages")

	// Retrieve all messages for the user
	filter := bson.M{
		"$and": []bson.M{
			{"$or": []bson.M{{"userID": userID}, {"deleteForMe": bson.M{"$ne": userID}}}},
			{"deleteForEveryOne": false}, // Exclude messages deleted for everyone
		},
	}

	cursor, err := collection.Find(context.Background(), filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())

	var messages []Message
	if err := cursor.All(context.Background(), &messages); err != nil {
		return nil, err
	}

	var history []string
	for _, msg := range messages {
		// Check if the current user has deleted the message
		if !contains(msg.DeleteForMe, userID) {
			history = append(history, fmt.Sprintf("%s|%s|%s", msg.UserID, msg.Content, msg.MessageID.Hex()))
		}
	}

	return history, nil
}

func UpdateMessage(messageID, userID, updatedContent string) error {
	client, err := initMongoDB()
	if err != nil {
		return err
	}

	defer client.Disconnect(context.Background())

	collection := client.Database("chat").Collection("messages")

	// Convert the string messageID to ObjectID if needed
	objID, err := primitive.ObjectIDFromHex(messageID)
	if err != nil {
		return err
	}
	updates := bson.M{
		"$set": bson.M{
			"content": updatedContent,
		},
	}

	filter := bson.M{
		"messageId": objID,
		"userID":    userID,
	}

	res, err := collection.UpdateOne(context.Background(), filter, updates, nil)
	if err != nil {
		fmt.Println("Error updating document:", err)
		return err
	}
	fmt.Println(res)

	// Notify all clients about the update
	updateMessage := fmt.Sprintf("%s|%s|%s|%s", userID, "/update", messageID, updatedContent)
	chatRoom.broadcast <- []byte(updateMessage)
	// Broadcast the updated message ID
	chatRoom.updateBroadcast <- messageID

	return err
}

// Helper function to check if a user ID is in the slice
func contains(slice []string, value string) bool {
	for _, v := range slice {
		if v == value {
			return true
		}
	}
	return false
}

func deleteForMe(messageID, userID string) error {
	client, err := initMongoDB()
	if err != nil {
		return err
	}

	defer client.Disconnect(context.Background())

	collection := client.Database("chat").Collection("messages")

	// Convert the string messageID to ObjectID if needed
	objID, err := primitive.ObjectIDFromHex(messageID)
	if err != nil {
		return err
	}

	updates := bson.M{
		"$addToSet": bson.M{
			"deleteForMe": userID,
		},
	}

	filter := bson.M{
		"messageId": objID,
	}

	fmt.Println("filter ->>", filter)
	res, err := collection.UpdateOne(context.Background(), filter, updates, nil)
	if err != nil {
		fmt.Println("Error updating document:", err)
		return err
	}
	fmt.Println(res)

	// Notify all clients about the deletion
	deletedMessage := fmt.Sprintf("%s|%s|%s", userID, "/delete_for_me", messageID)
	chatRoom.broadcast <- []byte(deletedMessage)

	// Broadcast the deleted message ID
	chatRoom.deleteBroadcast <- messageID

	return nil
}

func deleteForEveryOne(messageID, userID, loggedId string) error {
	fmt.Println("logged user id ->>", userID)
	client, err := initMongoDB()
	if err != nil {
		return err
	}

	defer client.Disconnect(context.Background())

	collection := client.Database("chat").Collection("messages")

	objID, err := primitive.ObjectIDFromHex(messageID)
	if err != nil {
		return err
	}

	updates := bson.M{
		"$set": bson.M{
			"deleteForEveryOne": true,
		},
	}
	filter := bson.M{
		"userID":            loggedId,
		"messageId":         objID,
		"deleteForEveryOne": false,
		// "deleteForMe": false, // Check that deleteForEveryOne is currently false
	}

	res, err := collection.UpdateOne(context.Background(), filter, updates, nil)
	if err != nil {
		return err
	}
	log.Println(res)

	// Notify all clients about the deletion
	deletedMessage := fmt.Sprintf("%s|%s|%s", userID, "/delete_for_everyone", messageID)
	chatRoom.broadcast <- []byte(deletedMessage)

	// Broadcast the deleted message ID
	chatRoom.deleteBroadcast <- messageID

	return nil
}

// Add a new function to save audio to MongoDB
func saveAudioToMongo(filePath string) error {
    client, err := initMongoDB()
    if err != nil {
        return err
    }
    defer client.Disconnect(context.Background())

	collection := client.Database("chat").Collection("messages")

    _, err = os.ReadFile(filePath)
    if err != nil {
        return err
    }

    audioMessage := Message{
        UserID:            "system", // You can customize this
        Content:           filePath,
        Timestamp:         time.Now(),
        MessageID:         primitive.NewObjectID(),
        DeleteForEveryOne: false,
    }

    _, err = collection.InsertOne(context.Background(), audioMessage)
    return err
}

func main() {
	app := fiber.New()

	// Initialize chat room
	go chatRoom.Run()

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendFile("./index.html")
	})

	app.Use("/ws", func(c *fiber.Ctx) error {
		// Get the user ID from the query parameters
		userID := c.Query("userId")
		if userID == "" {
			return fiber.NewError(fiber.StatusBadRequest, "User ID not provided")
		}

		// Load chat history from MongoDB when the user connects
		history, err := getAllMessages(userID)
		if err != nil {
			log.Println("Error fetching chat history:", err)
		}
		chatRoom.history = history

		// Pass the user ID and chat history to the WebSocket handler
		chatRoom.ServeWebSocket(c, userID)
		return nil
	})

	fmt.Println("Server started on :8080")
	err := app.Listen(":8080")
	if err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
}

