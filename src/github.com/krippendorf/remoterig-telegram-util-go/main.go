package main

import (
	"fmt"
	"flag"
	"strconv"
	"log"
	"time"
	"github.com/go-telegram-bot-api/telegram-bot-api"
	"strings"
	"net/http"
	"io/ioutil"
	"sync"
	"github.com/krippendorf/flex6k-discovery-util-go/src/github.com/krippendorf/flex6k-discovery-util-go/flex" /* hmmm..... ok*/
	"github.com/streadway/amqp"
	"encoding/json"
	"github.com/llgcode/draw2d/draw2dimg"
	"image/color"
	"math"
	"image"
	"bytes"
	"image/jpeg"
)

type AppContext struct {
	TelegramToken string
	TelegramChat  int64
	TelegramBot   *tgbotapi.BotAPI
	Rotor1216IP   string
	discoveryPackage flex.DiscoveryPackage
	rotationInProgress bool
	rabbitConnStr string
	lastFlexStatus time.Time
	sync.Mutex
}

type ListenerRegistration struct {
	listenerPort int
	listenerIp   string
	raw          string
	since        int64
}

const NDEF_STRING string = "NDEF"

func main() {
	context := new(AppContext)

	var chatIdString string

	flag.StringVar(&context.TelegramToken, "TOKEN", NDEF_STRING, "Telegram BOT API Token")
	flag.StringVar(&chatIdString, "CHAT", NDEF_STRING, "Telegram ChatID")
	flag.StringVar(&context.Rotor1216IP, "ROTOR1216", NDEF_STRING, "IP address of the 1216H Rotor Controller")
	flag.StringVar(&context.rabbitConnStr, "RABBITCONN", NDEF_STRING, "Rabbitmq connection string")
	flag.Parse()

	if(len(context.rabbitConnStr)>0){
		go consumeFlexRabbit(context)
	}

	chatIdInt, err := strconv.ParseInt(chatIdString, 10, 64);
	if (err != nil) {
		panic(fmt.Sprintf("ERROR %s", err))
	}

	context.TelegramChat = chatIdInt

	bot, err := tgbotapi.NewBotAPI(context.TelegramToken)
	if err != nil {
		log.Panic(err)
	}

	context.TelegramBot = bot
	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates, err := bot.GetUpdatesChan(u)

	time.Sleep(time.Second * 3)
	updates.Clear()

	for update := range updates {
		if update.Message == nil {
			continue
		}

		handleUpdate(&update, context)
	}

}

func consumeFlexRabbit(context *AppContext) {
	conn, err := amqp.Dial(context.rabbitConnStr)
	failOnError(err, "Failed to connect to RabbitMQ")
	defer conn.Close()

	ch, err := conn.Channel()
	failOnError(err, "Failed to open a channel")
	defer ch.Close()

	err = ch.ExchangeDeclare(
		"flex_topic", // name
		"topic",      // type
		true,         // durable
		false,        // auto-deleted
		false,        // internal
		false,        // no-wait
		nil,          // arguments
	)
	failOnError(err, "Failed to declare an exchange")

	q, err := ch.QueueDeclare(
		"",    // name
		false, // durable
		false, // delete when usused
		true,  // exclusive
		false, // no-wait
		nil,   // arguments
	)
	failOnError(err, "Failed to declare a queue")

	for _, s := range "#" {
		log.Printf("Binding queue %s to exchange %s with routing key %s",
			q.Name, "flex_topic", s)
		err = ch.QueueBind(
			q.Name,       // queue name
			"#",            // routing key
			"flex_topic", // exchange
			false,
			nil)
		failOnError(err, "Failed to bind a queue")
	}

	msgs, err := ch.Consume(
		q.Name, // queue
		"",     // consumer
		true,   // auto ack
		false,  // exclusive
		false,  // no local
		false,  // no wait
		nil,    // args
	)
	failOnError(err, "Failed to register a consumer")

	forever := make(chan bool)

	go func() {
		for d := range msgs {
			context.lastFlexStatus =  time.Now()
		    dec := json.NewDecoder(strings.NewReader(string(d.Body[:])))
			dec.Decode(&context.discoveryPackage)
		}
	}()

	<-forever
}

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
		panic(fmt.Sprintf("%s: %s", msg, err))
	}
}

func handleUpdate(update *tgbotapi.Update, context *AppContext) {

	if (update.Message.Chat.ID != context.TelegramChat) {
		return // only process messages from configured chat
	}

	if (strings.HasPrefix(update.Message.Text, "/flexstatus")) {

		if(context.discoveryPackage != flex.DiscoveryPackage{}){
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Last state: " + context.lastFlexStatus.Format(time.RFC850) +"\r\n Radio "+ context.discoveryPackage.Serial + " in state: '" + context.discoveryPackage.Status + "' " + context.discoveryPackage.Inuse_ip + " " + context.discoveryPackage.Inuse_host)
			msg.ReplyToMessageID = update.Message.MessageID
			context.TelegramBot.Send(msg)
		}else{
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Sorry, no idea...")
			msg.ReplyToMessageID = update.Message.MessageID
			context.TelegramBot.Send(msg)
		}
	}

	if (strings.HasPrefix(update.Message.Text, "/rotorstatus")) {
		stateDegree := getRotatorStatus(context)

		if(stateDegree >=0 && stateDegree<360){
			buf := new(bytes.Buffer)
			jpeg.Encode(buf, draw(stateDegree, -1), nil)
			b := tgbotapi.FileBytes{Name: "rotor.jpg", Bytes: buf.Bytes()}

			msgImage := tgbotapi.NewPhotoUpload(update.Message.Chat.ID, b)
			msgImage.ReplyToMessageID = update.Message.MessageID
			msgImage.Caption = fmt.Sprintf("Rotator is currently at %d°\n", stateDegree)
			context.TelegramBot.Send(msgImage)
		}else{
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("Rotator is currently at %d°\n", stateDegree))
			msg.ReplyToMessageID = update.Message.MessageID
			context.TelegramBot.Send(msg)
		}
	}

	if (strings.HasPrefix(update.Message.Text, "/setrotor")) {

		tokens := strings.Split(update.Message.Text, " ")

		if(len(tokens) != 2){
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("That is an invalid command"))
			msg.ReplyToMessageID = update.Message.MessageID
			context.TelegramBot.Send(msg)
			return
		}

		stateInt, err := strconv.Atoi(tokens[1])

		if(context.rotationInProgress){
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("Hey, wait my friend. Rotation is in progress!"))
			msg.ReplyToMessageID = update.Message.MessageID
			context.TelegramBot.Send(msg)
			return
		}

		if(err != nil || stateInt >360 || stateInt<1){
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("That is an invalid command. Range must be in 0-360°"))
			msg.ReplyToMessageID = update.Message.MessageID
			context.TelegramBot.Send(msg)
			return
		}

		stateDegree := getRotatorStatus(context)

		if(stateDegree >=0 && stateDegree<360){
			buf := new(bytes.Buffer)
			jpeg.Encode(buf, draw(stateDegree, stateInt), nil)
			b := tgbotapi.FileBytes{Name: "rotor.jpg", Bytes: buf.Bytes()}

			msgImage := tgbotapi.NewPhotoUpload(update.Message.Chat.ID, b)
			msgImage.ReplyToMessageID = update.Message.MessageID
			msgImage.Caption = fmt.Sprintf("Please wait, rotating from %d° to %d°\n", stateDegree, stateInt)
			context.TelegramBot.Send(msgImage)
		}else{
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("Please wait, rotating from %d° to %d°\n", stateDegree, stateInt))
			msg.ReplyToMessageID = update.Message.MessageID
			context.TelegramBot.Send(msg)
		}

		go rotateAndNotify(update, context, stateInt)
	}

}
func rotateAndNotify(update *tgbotapi.Update, context *AppContext, i int) {
	context.rotationInProgress = true;
	getHttpString("http://" + context.Rotor1216IP + "/rotatorcontrol/set/" + strconv.Itoa(i))

	seconds := 0;

	for {

		if(seconds > 180){
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("Rotation timed out - status is %d°\n", i))
			msg.ReplyToMessageID = update.Message.MessageID
			context.TelegramBot.Send(msg)
			context.rotationInProgress = false;
			return
		}

		if(getRotatorStatus(context) == i){


			buf := new(bytes.Buffer)
			jpeg.Encode(buf, draw(i, -1), nil)
			b := tgbotapi.FileBytes{Name: "rotor.jpg", Bytes: buf.Bytes()}

			msgImage := tgbotapi.NewPhotoUpload(update.Message.Chat.ID, b)
			msgImage.ReplyToMessageID = update.Message.MessageID
			msgImage.Caption = fmt.Sprintf("Rotation done, we're now looking at %d°\n", i)
			context.TelegramBot.Send(msgImage)

			context.rotationInProgress = false;
			return
		}

		seconds++;

		time.Sleep(time.Second * 2)
	}

	context.rotationInProgress = false; // should not happen... anyway....

}

func getHttpString(url string) (responseString string) {

	resp, err := http.Get(url)

	if (err != nil) {
		fmt.Printf("HTTP GET ERR: %s\n", err)
	}

	if resp.StatusCode == 200 {
		bodyBytes, err2 := ioutil.ReadAll(resp.Body)

		if (err2 != nil) {
			fmt.Printf("HTTP GET ERR: %s\n", err2)
		}

		responseString = string(bodyBytes)
	}
	return
}

func getRotatorStatus(context *AppContext) (deg int) {
	deg = 1000
	powerOn := getHttpString("http://" + context.Rotor1216IP + "/rotatorcontrol/set/power/on")

	if (len(powerOn) == 0) {
		fmt.Printf("power/on operation failed\n")
	}

	getResult := getHttpString("http://" + context.Rotor1216IP + "/rotatorcontrol/get")

	if (len(getResult) == 0) {
		fmt.Printf("/rotatorcontrol/get operation failed\n")
	}

	fmt.Printf("----> rotatorcontrol/get %s\n", getResult)

	tokens := strings.Split(getResult, "|")

	stateInt, err := strconv.Atoi(tokens[3])

	if (err != nil) {
		fmt.Printf("HTTP GET ERR: %s\n", err)
	} else {
		deg = stateInt;
	}

	return
}

func draw(from int, to int) *image.RGBA{

	dest := image.NewRGBA(image.Rect(0, 0, 600, 600))
	source, _ := draw2dimg.LoadFromPngFile("/flexi/locator.png")
	gc := draw2dimg.NewGraphicContext(dest)
	gc.DrawImage(source)

	addLine(gc,5, color.NRGBA{0x33, 255, 0x33, 0x80}, float64(from))

	if(to>=0){
		addLine(gc,5, color.NRGBA{255, 0x33, 0x33, 0x80}, float64(to))
	}

	gc.Restore()
	return dest
}

func addLine(gc *draw2dimg.GraphicContext, i int, nrgba color.NRGBA, target float64) {
	targetAngle := (-90 + target) * (math.Pi / 180.0)
	gc.SetFillColor(nrgba)
	gc.SetStrokeColor(nrgba)
	gc.SetLineWidth(15)

	gc.MoveTo(300+math.Cos(targetAngle)*280.0, 300+math.Sin(targetAngle)*280.0)
	gc.LineTo(300, 300)
	gc.Stroke()
}