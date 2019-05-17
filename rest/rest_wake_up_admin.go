/*
Format Json
{
"telnumber": "xxxxxxxx"
"message": "Внимание   все сломалось"
}
авторизация по headers
headers = {
        'Authorization': 'Bearer ' + iam_token,
}
entry points:
/api/v1/makecall
/api/vi/getstatus/{issue_num}
Предусмотреть  разные движки  для TTS (Polly и Yandex)
Яндекс протестить
Думаю  стоит  сделать отдельный  микросервис который генерит файлы для Asterisk
Прикрутить  jwt авторизацию
3 типа  звонков

1) Text-To-Voice    		(Done)
2) Playback  with files 	(Частично)
3) Bridge (Tel1 <-> Aster <-> Tel2 ) с записью разговоров (Done)
4) -- ?

короч  не будет  никаких  postgres,  а будет одно сплошное телевидение, т е Redis и хеши . Храним все там
Еще  нужно  сделать  route  для  звонка с  указанием  пира  через  который  нужно  звонить
{
	"endpoint" : "/SIP/ncc/"
}
Инициализацию  очередей - QueueDeclare  - rabbitmq  перенести на старт программы . Подумать как это сделать лучше...
*/
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"

	//_ "github.com/lib/pq"
	"github.com/joho/godotenv"

	"github.com/Sirupsen/logrus"

	//"context"

	"os"
	"path/filepath"
	"time"

	"github.com/asaskevich/govalidator"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/cors"
	"github.com/go-chi/render"

	//"github.com/prometheus/common/log"
	"github.com/go-redis/redis"
	//_ "github.com/lib/pq"
	uuid "github.com/satori/go.uuid"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/streadway/amqp"
)

type ctxKeyID int

// RequestIDKey is the key that holds the unique request ID in a request context.
const IDKey ctxKeyID = 1
const PLAYFILE = "474e82fd-1337-43be-9d78-0c1d06b7d3d3"

type IsOk int

const (
	OK IsOk = iota
	FAIL
)

type Configuration struct {
	AuthKey          string `mapstructure:"authkey"`
	HostDbServer     string `mapstructure:"host_db_server"`
	HostRabbitServer string `mapstructure:"host_rabbit_server"`
	DbName           int    `mapstructure:"dbname"` //используем   redis  а там база это число
	DbUser           string `mapstructure:"dbuser"`
	DbPassword       string `mapstructure:"dbpassword"`
	RabbitUser       string `mapstructure:"rabbit_user"`
	RabbitPassword   string `mapstructure:"rabbit_password"`
	SoundFileDir     string `mapstructure:"sound_file_dir"`
}

//
type AnswerToRabbit struct { //структура  которую получаем в качестве  результата  звонка

	HTTPStatusCode int       `json:"-"` // http response status code
	Issue          string    `json:"issue"`
	FirstPhone     string    `json:"first_phone"`
	SecondPhone    string    `json:"second_phone,omitempty"`
	AnswerFirst    Answer1   `json:"answer_first,omitempty"`
	AnswerSecond   Answer1   `json:"answer_second,omitempty"` // будет  пропускаться при конвертации из  JSON в структуру
	LastCallTime   time.Time `json:"last_call_time"`          //здесь будем  хранить последнее время звонка (на Первый номер в случае bridge)
	SuccessDialog  bool      `json:"success_dialog"`          //это можно использовать для всего диалога
}

func (a *AnswerToRabbit) Render(w http.ResponseWriter, r *http.Request) error { //нужно для реализации интерфейса renderer
	render.Status(r, a.HTTPStatusCode)
	return nil
}

type Answer1 struct { //если  нужно будет хранить  какие то другие статусы  добавим их сюда
	SuccessCall bool `json:",omitempty"`
}

type ErrResponse struct {
	Err            error `json:"-"` // low-level runtime error
	HTTPStatusCode int   `json:"-"` // http response status code

	StatusText string `json:"status"`          // user-level status message
	AppCode    int64  `json:"code,omitempty"`  // application-specific error code
	ErrorText  string `json:"error,omitempty"` // application-level error message, for debugging
}

type CallRequest struct { //это для POST запроса  UUID  генерим автоматом
	ID              string //id  мы генерим и устанавливаем вручную
	Telnumber       string `json:"telnumber" valid:"telNumber"`                 //кастомная  валидация  на те номер (длина строки и все символы цифры)
	Message         string `json:"message"`                                     //здесь либо  тест  который нужно  преобразовать в голос  либо имя файла которое нужно проиграть
	Type            string `json:"type" valid:"in(text|file|bridge)"`           //это тип  запроса {text, file, bridge} нужно подставить автоматом
	SecondTelnumber string `json:"second_telnumber" valid:"telNumber,optional"` //  второй  номер куда звоним  может быть прорущен

}

//  Эту структуру  мы  передаем в rabbit для запроса playback
type IssuesClients struct {
	Issue             string   `json:"issue"` //issue  нам таки нужен Будем генерить  его на Rest автоматом. Он нужен в том числе для организации повторного звонка
	Phone             string   `json:"phone"`
	VoiceMessageFiles []string `json:"files"` //здесь файлы которые надо проиграть
}

//  Эту структуру  мы  передаем в rabbit для  запроса  bridge
type IssuesBridge struct { //запрос на соединение 2-х абонентов
	Issue       string `json:"issue"` //issue  нам таки нужен Будем генерить  его на Rest автоматом. Он нужен в том числе для организации повторного звонка
	FirstPhone  string `json:"first_phone"`
	SecondPhone string `json:"second_phone"`
}

type RequestFile struct { //структура  для запроса создания файла
	Issue   string `json:"issue"`   // по этому issue сгенерим назв файла
	Message string `json:"message"` //здесь сообщение которое нужно озвучить
}

//
func (a *CallRequest) Bind(r *http.Request) error { //это  нужно  для  реализации интерфейса  Binder
	fmt.Println("In  bind  Call Request")

	if a.Telnumber == "" {
		return errors.New("Missing required telnumber field.")
	}
	if a.Type == "" {
		return errors.New("Missing required TYPE field.")
	}
	fmt.Println("Type", a.Type)
	if a.Message == "" && a.Type == "text" { //если  тип  звонка  text  и message не задан то генерим ошибку
		return errors.New("Missing required message field.")
	}

	if a.SecondTelnumber == "" && a.Type == "bridge" { //если  тип  звонка  text  и message не задан то генерим ошибку
		log.Info(a.SecondTelnumber)
		return errors.New("Missing required second_telnumber field.")
	}

	//
	_, err := govalidator.ValidateStruct(a) //попробуем сделать  валидацию параметров
	if err != nil {
		fmt.Println("Ошибка : ", err)
		wrongFields := []string{}
		if allErrs, ok := err.(govalidator.Errors); ok { // приводим  результат ошибки  к  govalidator.Errors  здесь все ошибки валидации
			for _, fld := range allErrs.Errors() {
				//data := []byte(fmt.Sprintf("field: %#v\n\n", fld))
				if wrongFld, ok := fld.(govalidator.Error); ok { //безопасное  приведение типа
					//  приводим  текущую  ошибку  к   нужному  типу  и получаем  имя  поля с ошибкой валидации
					fmt.Println(wrongFld.Validator)
					fmt.Println(wrongFld)
					wrongFields = append(wrongFields, wrongFld.Validator)
					//w.Write(data)
					fmt.Println(wrongFields)
				} else {
					fmt.Println("fld НЕ смогли сделать  кастинг")
					fmt.Println("Ошибки  при  валидации : ", allErrs.Errors())
				}
			}
		}
		return errors.New("Missing  parameters" + " " + err.Error())
	}
	fmt.Println("Exit from bind")
	a.ID = GetID(r.Context()) //пока так , потом сделать проверку!!!!  Устанавливаем ID

	return nil
}

//
type Query struct { //это для get запроса и разбора параметров. может потом в запросе что то еще появится

	Recipient string `schema:"telnumber" valid:"telNumber"`
}

type Answer struct { // это для ответа клиенту что типа приняли запрос
	Message string `json:"message"`
}

type AnswerWithId struct { // это для ответа клиенту что типа приняли запрос
	Id      string `json:"id"`
	Message string `json:"message"`
}

func (a *AnswerWithId) Render(w http.ResponseWriter, r *http.Request) error {
	// Pre-processing before a response is marshalled and sent across the wire
	a.Message = "Call is successfuly quered"
	return nil
}

func NewAnswerWithId(message string, id string) *AnswerWithId { //функция для генерации ответа
	resp := &AnswerWithId{Message: message, Id: id}
	return resp
}

func (a *Answer) Render(w http.ResponseWriter, r *http.Request) error {
	// Pre-processing before a response is marshalled and sent across the wire
	a.Message = "Call is successfuly quered"
	return nil
}

func NewAnswer(message string) *Answer { //функция для генерации ответа
	resp := &Answer{Message: message}
	return resp
}

func init() {
	govalidator.CustomTypeTagMap.Set("telNumber", govalidator.CustomTypeValidator(func(i interface{}, o interface{}) bool { //устанавливаем кастомный валидатор
		telnum, ok := i.(string)
		if !ok {
			return false
		}
		//такая  валидация тел номера немного ошибочна но нам пох т. к. все равно мы ПОЛЮБАСУ ображим 10 последние цифр и склеим с префиксом из конфига (8 или 7)
		// распознаем +7918... , 7918, 8918
		re := regexp.MustCompile(`^[+]*[0-9]{11}$`)
		if re.MatchString(telnum) {
			return true

		} else {
			return false
		}
		// if len(telnum) != 10 || !govalidator.IsNumeric(telnum) {
		// 	return false
		// }
		// return true

	}))

}

func AuthRest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//isAdmin, ok := r.Context().Value("acl.admin").(bool)
		//token, ok := r.Header.Get("Authorization")
		token := r.Header.Get("Authorization")
		if token != configurationInst.AuthKey {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var ErrNotFound = &ErrResponse{HTTPStatusCode: 404, StatusText: "Resource not found."}

var configurationInst Configuration
var conn *amqp.Connection
var log = logrus.New()
var transferToRabbit = make(chan *CallRequest, 50) //канал  для  связи  с рутиной  которая занимается отправкой данных в RabbitMQ и созданием звук файлов
var RedisClient *redis.Client                      //возможно  будем ходить  в redis  из разных Go routine
var db *sql.DB                                     //наш connection pool

func main() {
	//var prod *viper.Viper
	var configFile = flag.String("c", "config.json", "config file") //по умолчанию  config.json
	flag.Parse()

	//Проверяем и читаем конфиг

	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Fatal(err)
	}

	//вот здесь уже нужно  вернуть viper объект  для Unmarshal
	currentConfig, err := readConfig(filepath.Join(dir, *configFile))
	//вот  здесь  возвращаем что нужно  конец функции
	err = currentConfig.Unmarshal(&configurationInst)
	if err != nil {
		fmt.Printf("unable to decode into struct, %v", err)
		log.Fatalf("unable to decode into struct, %v", err)
	}
	fmt.Printf("структура:  %+v", configurationInst)

	//
	r := chi.NewRouter()

	//r.Use(middleware.RequestID) //этот  id  потом отдадим  в rest
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	//r.Use(middleware.URLFormat) //middleware.URLFormatCtxKey
	r.Use(render.SetContentType(render.ContentTypeJSON))
	// устанавливаем  redis  client  Пока  будем использовать redis  потом может что то другое
	RedisClient = redis.NewClient(&redis.Options{ // киент Redis concurently Safe и может быть использован из разных  go routine
		Addr:         configurationInst.HostDbServer + ":6379", //нужно ли порт в конфиг ?
		Password:     "",                                       // no password set
		DB:           configurationInst.DbName,                 // use default DB
		PoolSize:     100,
		MinIdleConns: 20,
	})

	pong, errRedis := RedisClient.Ping().Result()
	if errRedis == nil {
		fmt.Println("redis is avalible")
		fmt.Println(pong)
	} else {
		fmt.Println("redis unavalible")
		log.Error("redis unavalible")
		os.Exit(3)
	}
	//
	cors := cors.New(cors.Options{
		// AllowedOrigins: []string{"https://foo.com"}, // Use this to allow specific origin hosts
		AllowedOrigins: []string{"*"},
		// AllowOriginFunc:  func(r *http.Request, origin string) bool { return true },
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		//AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		//ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300, // Maximum value not ignored by any of major browsers
	})

	// есть 2 варианта: либо  раскидать по нескольким url типы звонков , либо указывать type в параметра запроса .
	// таки я выбираю 2 вариант, пускай type указывает клиент api. хотя...  надо будет подумать над этим

	r.Route("/api/v1/{issue}", func(r chi.Router) { // articleID надо  провалидировать на предмет длины 10 и все символы цифры
		r.Use(cors.Handler)
		//r.Use(AuthRest)       //
		r.Get("/", GetStatus) //

	})

	r.Route("/api/v1/make_call", func(r chi.Router) {
		r.Use(cors.Handler)
		r.Use(AuthRest)       //
		r.Use(AddId)          //здесь   генерим UUID и этот id потом как строку (в json ) отдаем в rabbit
		r.Post("/", MakeCall) //  сделать  звонок

	})

	go func() { // это  рутина  для отправки  сообщения  в rabbit и для генерации файла озвучки
		var err error

		//еще  нужно генерить какой то id  например uuid и логировать соответствие requestid- uuid
		//uuid потом пригодится для  идентификации звонка
		//здесь  будет 2 канала  один для отправки в rabbit , другой для принятия сообщений от кролика о дозвоне или недозвоне

		//
		for { //внешгий  цикл for
			//	fmt.Println("Connecting...")
			time.Sleep(1 * time.Second)
		OnError:
			for {
				fmt.Println("Connecting RabbitMQ...")
				connectionString := fmt.Sprintf("amqp://%[1]v:%[2]v@%[3]v:5672/", configurationInst.RabbitUser, configurationInst.RabbitPassword, configurationInst.HostRabbitServer)
				fmt.Println(connectionString)
				conn, err = amqp.Dial(connectionString)
				//conn, err = amqp.Dial("amqp://" + configurationInst.RabbitUser + ":" + configurationInst.RabbitPassword + "@" + configurationInst.HostRabbitServer + ":5672/")
				if err != nil {
					log.Println(err)
					time.Sleep(3 * time.Second)
					fmt.Println("Reconnecting  RabbitMQ....")
					continue
				} else {
					break OnError
				}

			}
			//time.Sleep(1 * time.Second)
			fmt.Println("Connected  RabbitMQ!!!")
			rabbitCloseError := make(chan *amqp.Error)
			notify := conn.NotifyClose(rabbitCloseError) //error channel
			//failOnError(err, "Failed to connect to RabbitMQ")
			//defer conn.Close()
			//Объявляем  канал для Consumer
			ch, err := conn.Channel() //channel for consumer
			failOnError(err, "Failed to open a channel")
			chPub, err := conn.Channel() //channel for Pub перенес  сюда инициализацию
			failOnError(err, "Failed to open a channel")
			err = ch.Qos( //Qos  for consumer
				1,     // prefetch count
				0,     // prefetch size
				false, // global
			)
			failOnError(err, "Failed to set QoS")
			defer ch.Close()
			//объявляем  точку  обмена
			err = chPub.ExchangeDeclare(
				"dialerInput", // name
				"direct",      // type
				true,          // durable
				false,         // auto-deleted
				false,         // internal
				false,         // no-wait
				nil,           // arguments
			)
			failOnError(err, "Failed to declare  exchange  dialerInput")
			//
			q, err := ch.QueueDeclare( //queue   declare for CONSUMER!!
				"resultsDialer", // name
				true,            // durable
				false,           // delete when unused
				false,           // exclusive
				false,           // no-wait
				nil,             // arguments
			)
			failOnError(err, "Failed to declare a queue")
			//BIND  делается на  ПРИЕМНИКЕ !!!
			err = ch.QueueBind("resultsDialer", "resultsDialer", "dialerInput", false, nil) //привязываем очередь  результатов к Exchange point
			failOnError(err, "Failed to bind  queue dialer")
			msgs, err := ch.Consume( //consumer messages
				q.Name, // queue
				"",     // consumer
				true,   // auto-ack  //Внимание вот здесь надо сделать ручное подтверждение!!!
				false,  // exclusive
				false,  // no-local
				false,  // no-wait
				nil,    // args
			)
			failOnError(err, "Failed to register a consumer")
			var d amqp.Delivery
			// конец

			//chPub, err := conn.Channel() //channel for Pub
			//failOnError(err, "Failed to open a channel")
			//объявляем  точку   обмена
			// err = chPub.ExchangeDeclare(
			// 	"dialerInput", // name
			// 	"direct",      // type
			// 	true,          // durable
			// 	false,         // auto-deleted
			// 	false,         // internal
			// 	false,         // no-wait
			// 	nil,           // arguments
			// )
			// failOnError(err, "Failed to declare  exchange")
			///
			//chPub, err := conn.Channel() //channel for Publisher
			//failOnError(err, "Failed to open a channel chPub")
			//  публикация
			// err = ch.Publish(
			// 	"",       // exchange
			// 	"dialer", // routing key  Здесь
			// 	false,    // mandatory
			// 	false,    // immediate
			// 	amqp.Publishing{
			// 		ContentType: "application/json",
			// 		Body:        []byte(body),
			// 	})

			// if err != nil {
			// 	log.Info(err)
			// }
			//  конец  публикации

			defer chPub.Close()
			//
			//теперь  здесь вместо  for range используем for select
			//for message := range answerToRabbitCh {
			//Здесь  нужно  проверить тип
			//
		GoReconnect:
			for { //внутренний цикл
				select {
				case err = <-notify:
					fmt.Println("LOST RabbitMQ  CONNECTION !!!")
					break GoReconnect //reconnect
				case message := <-transferToRabbit: //здесь  получаем структуру
					//вот  сюда  придут  все 3 вида  запроса  на звонок  их  нужно  маршрутизировать в зависимости от type
					if message.Type == "text" { // сделать  разделение  text и alert

						newRequest := &IssuesClients{VoiceMessageFiles: make([]string, 1)} //создаем пустую структру  для запроса
						newRequest.Phone = message.Telnumber
						newRequest.Issue = message.ID //используем сей час для генерации имени файла
						newRequest.VoiceMessageFiles[0] = message.ID
						fmt.Println("Message from REST:", message.Message)
						//генерация  файла озвучки //
						// err := makeVoiceFile(ctx_timeout, message.Message, newRequest.Issue, configurationInst.SoundFileDir)
						// if err != nil {
						// 	fmt.Println("файл Озвучки  не создан")
						// 	continue
						// }
						//для дебага пробуем испытать rpc
						//makeVoiceFileRPC(message string, issue string, username string, password string, rabbitServer string) (res string, err error)
						ok, err := makeVoiceFileRPC(message.Message, message.ID, configurationInst.RabbitUser, configurationInst.RabbitPassword, configurationInst.HostRabbitServer)
						if err != nil { //файла  озвучки нет  поэтому делаем default  оповещение
							fmt.Println("ошибка в вызове RPC функции")
							newRequest.VoiceMessageFiles[0] = "default_alert" //default  alert
							log.Info(err)
						}
						//вот здесь  нужно проанализировать ok  и если FAIL заменить newRequest.VoiceMessageFiles[0] на default_alert
						fmt.Println("Результат: ", ok) //ok  теперь это MAP
						if ok["Result"] == "FAIL" {    //если  что то пошло не так и файл озвучки не создан делаем звонок с default_alert
							newRequest.VoiceMessageFiles[0] = "default_alert"
						} else {
							newRequest.VoiceMessageFiles[0] = ok["FileName"]
						}
						body, err := json.Marshal(newRequest)
						if err != nil {
							fmt.Println("Не удалось создать JSON")
							log.Info(err)
						}

						//
						err = chPub.Publish(
							"dialerInput", // exchange
							"dialer",      // routing key  Здесь
							false,         // mandatory
							false,         // immediate
							amqp.Publishing{
								ContentType: "application/json",
								Body:        []byte(body),
							})

						if err != nil {
							log.Info(err)
						}
						fmt.Println("In  ANSWER to Rabbit channel : ", message)
					} else if message.Type == "bridge" {
						//
						newRequest := &IssuesBridge{} //создаем пустую структру  для запроса
						newRequest.FirstPhone = message.Telnumber
						newRequest.SecondPhone = message.SecondTelnumber
						newRequest.Issue = message.ID //используем сей час для генерации имени файла

						//генерация  файла озвучки //
						body, err := json.Marshal(newRequest)
						if err != nil {
							fmt.Println("Не удалось создать JSON")
							log.Info(err)
						}

						//
						err = chPub.Publish(
							"dialerInput",  // exchange
							"dialerBridge", // routing key  Здесь
							false,          // mandatory
							false,          // immediate
							amqp.Publishing{
								ContentType: "application/json",
								Body:        []byte(body),
							})

						if err != nil {
							log.Info(err)
						}
						fmt.Println("In  ANSWER to Rabbit channel : ", message)
						//
					} else if message.Type == "file" {
						//
						newRequest := &IssuesClients{VoiceMessageFiles: make([]string, 1)} //создаем пустую структру  для запроса
						newRequest.Phone = message.Telnumber
						newRequest.Issue = message.ID                     //используем сей час для генерации имени файла
						newRequest.VoiceMessageFiles[0] = message.Message //если тип message то здесь имя файла
						fmt.Println("Message from REST:", message.Message)
						//генерация  файла озвучки //
						body, err := json.Marshal(newRequest)
						if err != nil {
							fmt.Println("Не удалось создать JSON")
							log.Info(err)
						}

						//
						err = chPub.Publish(
							"dialerInput", // exchange
							"dialer",      // routing key  Здесь
							false,         // mandatory
							false,         // immediate
							amqp.Publishing{
								ContentType: "application/json",
								Body:        []byte(body),
							})

						if err != nil {
							log.Info(err)
						}
						fmt.Println("In  ANSWER to Rabbit channel : ", message)
						//
					} else {
						log.Info("Unexpected  message type. Message  %+v: ", message)
						//здесь нужно  записать в redis результат что нихуя не получилочь  позвонить
						continue
					}
					//делаем еще один  case с каналом по результатам и пишем результат в субд
				case d = <-msgs: //здесь  получим  результаты  их  надо записать в redis
					fmt.Println("В канале получения  сообщения о результате")
					//
					var AnswerToRabbitInst = new(AnswerToRabbit) //создаем конфиг по issue

					log.Printf("Received a message in answer: %s", d.Body)
					err := json.Unmarshal([]byte(d.Body), &AnswerToRabbitInst)
					if err != nil {
						fmt.Println("Error:", err)
						//log.Fatalf("%s: %s", d.Body, err)
						d.Ack(false) //подтверждаем ТОЛЬКО текщее сообщение  вручную
						continue
					}
					fmt.Println(AnswerToRabbitInst)
					// Делаем вставку  в Postgres
					dataJson, _ := json.Marshal(AnswerToRabbitInst)
					_, err = RedisClient.HSet(AnswerToRabbitInst.Issue, "data", dataJson).Result()
					if err != nil {
						log.Error("Error while  insert  result  data")

					}
				}
			}

		}

	}()

	log.Fatal(http.ListenAndServe(":3333", r)) //запускаемся с нашими роутами

}

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
	}
}

func MakeCall(w http.ResponseWriter, r *http.Request) { //

	//
	//data := &CallRequest{}                       //  создаем  пустую структуру
	fmt.Println("Request URI is : ", r.RequestURI)
	data := new(CallRequest)
	if err := render.Bind(r, data); err != nil { //Тип  данных с которым мы работаем должен реализовывать  интерфейс  binder (и render если если хотим над ним делать render.Render)
		render.Render(w, r, ErrInvalidRequest(err)) //и вначале  парсим запрос
		return
	}
	fmt.Println("In create call : ")
	fmt.Println("Current request id is equal : ", GetID(r.Context())) //это id уникальный который генерим и используем для id звонка и id файла
	//ID  вставляем в интерфейсе Binder
	fmt.Println("Data after all transformations: ", data)
	//теперь  эту структуру можно смело отправлять в канал
	transferToRabbit <- data //отправляем данные в rabbit и возвращаем OK

	render.Status(r, http.StatusCreated) //говорим что все ок
	//говорим что приняли запрос и отдаем id по которому ПОТОМ  можно спросить
	render.Render(w, r, NewAnswerWithId("Call is successfuly queued", GetID(r.Context())))

	//

}

func GetStatus(w http.ResponseWriter, r *http.Request) { //делаем  запрос по issue  выводим  структуру
	// if err := render.Render(w, r, NewArticleListResponse(articles)); err != nil {
	// 	render.Render(w, r, ErrRender(err))
	// 	return
	// }
	//вначале разбираем параметры
	if issue := chi.URLParam(r, "issue"); issue != "" {
		//здесь проверяем и отвечаем
		log.Info("Issue is equal: ", issue)
		if ok, _ := RedisClient.HExists(issue, "data").Result(); ok {
			fmt.Println(":")
			raw_data, err := RedisClient.HGet(issue, "data").Result() //здесь должен быть json
			log.Info("В функции get!!!")
			log.Info("Raw data :", raw_data)
			if err != nil && raw_data != "" {
				log.Error("In getstatus")
				var ErrNotFound = &ErrResponse{HTTPStatusCode: 404, StatusText: "Issue not found."}
				render.Render(w, r, ErrNotFound)
			} else {
				//respondwithJSON(w, r, http.StatusOK, raw_data) //здесь получаем ошибку!
				log.Info("в GetStatus!!!")
				w.Write([]byte(raw_data))

			}

		} else {
			render.Render(w, r, ErrNotFound)
			return
		}

		// raw_data, err := RedisClient.HGet(issue, "data").Result() //здесь должен быть json
		// log.Info("В функции get!!!")
		// log.Info("Raw data :", raw_data)
		// if err != nil && raw_data != "" {
		// 	log.Error("In getstatus")
		// 	var ErrNotFound = &ErrResponse{HTTPStatusCode: 404, StatusText: "Issue not found."}
		// 	render.Render(w, r, ErrNotFound)
		// } else {
		// 	//respondwithJSON(w, r, http.StatusOK, raw_data) //здесь получаем ошибку!
		// 	log.Info("в GetStatus!!!")
		// 	w.Write([]byte(raw_data))

		// }

	} else {
		render.Render(w, r, ErrNotFound)
		return
	}

}

func (e *ErrResponse) Render(w http.ResponseWriter, r *http.Request) error {
	render.Status(r, e.HTTPStatusCode)
	return nil
}

func ErrInvalidRequest(err error) render.Renderer {
	return &ErrResponse{
		Err:            err,
		HTTPStatusCode: 400,
		StatusText:     "Invalid request.",
		ErrorText:      err.Error(),
	}
}

func ErrRender(err error) render.Renderer {
	return &ErrResponse{
		Err:            err,
		HTTPStatusCode: 422,
		StatusText:     "Error rendering response.",
		ErrorText:      err.Error(),
	}
}

func AddId(next http.Handler) http.Handler { //функция  для  добавления  контекста  с ID запроса
	var err error
	fn := func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		requestID := r.Header.Get("X-Phone-Id")
		if requestID == "" {
			myid := uuid.Must(uuid.NewV4(), err) //этот  uuid  используем потом  для имени файла озвучки и идентификации запроса
			requestID = fmt.Sprintf("%s", myid)
			fmt.Println(requestID)
		}
		ctx = context.WithValue(ctx, IDKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
	return http.HandlerFunc(fn)
}

// GetReqID returns a request ID from the given context if one is present.
// Returns the empty string if a request ID cannot be found.
func GetID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if reqID, ok := ctx.Value(IDKey).(string); ok {
		return reqID
	}
	return ""
}

//
func stringInSlice(a string, list []string) bool { //  Пока гн тмпользуется эта функция  на будующее для кастомной валидации вместо валидатора  IN в структуре
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

func respondwithJSON(w http.ResponseWriter, r *http.Request, code int, payload interface{}) {
	//
	var AnswerToRabbitInst = new(AnswerToRabbit) //создаем конфиг по issue
	payload_in_byte, ok := payload.(*[]byte)
	if !ok {
		//здесь обработаем ошибку
		var ErrNotFound = &ErrResponse{HTTPStatusCode: 404, StatusText: "Issue not found."}
		render.Render(w, r, ErrNotFound)

	}

	log.Printf("Received a message in response JSON: %s", payload)
	err := json.Unmarshal(*payload_in_byte, &AnswerToRabbitInst)
	if err != nil {
		fmt.Println("Error:", err)
		log.Fatalf("%s", err)

	}
	fmt.Printf("%+v", AnswerToRabbitInst)

	dataJson, _ := json.Marshal(AnswerToRabbitInst)
	_, err = RedisClient.HSet(AnswerToRabbitInst.Issue, "data", dataJson).Result()
	if err != nil {
		log.Error("Error while  insert  result  data")

	}

	//
	response, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(response)
}

/////
func randInt(min int, max int) int {
	return min + rand.Intn(max-min)
}

//
func randomString(l int) string {
	bytes := make([]byte, l)
	for i := 0; i < l; i++ {
		bytes[i] = byte(randInt(65, 90))
	}
	return string(bytes)
}

/////////////////////////////функция  для вызова по rpc///////////////
//нужно  сделать что бы она возвращала  либо объект, либо  map и передавала  туда  имя файла для проигрывания
// это нужно для организации кеширования  можно  например func MyFunc(....) map[string]string, err
func makeVoiceFileRPC(message string, issue string, username string, password string, rabbitServer string) (map[string]string, error) {
	//func makeVoiceFileRPC(message string, issue string, username string, password string, rabbitServer string) (res string, err error) {
	answerRpc := make(map[string]string)

	var RequestInst = new(RequestFile) //создаем конфиг по issue
	RequestInst.Message = message
	RequestInst.Issue = issue
	body, err := json.Marshal(RequestInst)
	if err != nil {
		fmt.Println("Не удалось создать JSON")
		log.Info(err)
	}

	//conn, err := amqp.Dial("amqp://guest:guest@localhost:5672/")
	connectionString := fmt.Sprintf("amqp://%[1]v:%[2]v@%[3]v:5672/", username, password, rabbitServer)
	fmt.Println(connectionString)
	conn, err = amqp.Dial(connectionString)
	if err != nil {
		fmt.Println("Error:", err)

		return answerRpc, errors.New("Failed to connect to RabbitMQ")
	}

	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		fmt.Println("Error:", err)

		return answerRpc, errors.New("Failed to open a channel")
	}

	defer ch.Close()

	q, err := ch.QueueDeclare(
		"",    // name
		false, // durable
		false, // delete when usused
		true,  // exclusive
		false, // noWait
		nil,   // arguments
	)
	if err != nil {
		fmt.Println("Error:", err)

		return answerRpc, errors.New("Failed to declare a queue")
	}

	msgs, err := ch.Consume(
		q.Name, // queue
		"",     // consumer
		true,   // auto-ack
		false,  // exclusive
		false,  // no-local
		false,  // no-wait
		nil,    // args
	)
	if err != nil {
		fmt.Println("Error:", err)

		return answerRpc, errors.New("Failed to register a consumer")
	}

	err = ch.Publish(
		"",          // exchange
		"rpc_queue", // routing key
		false,       // mandatory
		false,       // immediate
		amqp.Publishing{
			ContentType:   "application/json",
			CorrelationId: issue, //issue  id uniq!!
			ReplyTo:       q.Name,
			Body:          []byte(body),
		})
	if err != nil {
		fmt.Println("Error:", err)

		return answerRpc, errors.New("Failed to publish a message")
	}

	for {
		select {
		case message := <-msgs: //здесь  json
			var resData = make(map[string]string) //может лучше  в структуру  мапить? еще здесь  нужно возвращать имя файла
			err = json.Unmarshal([]byte(message.Body), &resData)
			if err != nil {
				fmt.Printf("unable to decode into struct, %v", err)
				//return "", errors.New("unable to decode into struct")
				return answerRpc, errors.New("unable to decode into struct")
			}
			fmt.Println("Recieve  result!!!")
			if issue == message.CorrelationId && resData["Answer"] == "OK" {
				fmt.Println(" Result  IS:", string(message.Body))
				fmt.Println(" OK!!!!")
				answerRpc["Result"] = "OK"
				answerRpc["FileName"] = resData["FileName"]
				return answerRpc, nil
			} else if issue == message.CorrelationId && resData["Answer"] == "FAIL" {
				fmt.Println(" Result  IS:", string(message.Body))
				fmt.Println("FAIL!!!")
				answerRpc["Result"] = "FAIL"
				answerRpc["FileName"] = ""
				return answerRpc, nil
			} else {
				fmt.Println("FAIL IN ELSE!!!")
				answerRpc["Result"] = "FAIL"
				answerRpc["FileName"] = ""
				return answerRpc, nil
				//return "FAIL", nil
			}
		case <-time.After(50 * time.Second): //  если   не дождались ответа от RPC  вызова
			answerRpc := make(map[string]string)
			return answerRpc, errors.New("Timeout  while  wait results from make file")
		}
	}

}

//Function for reading config file. return  viper object and error
func readConfig(filename string) (*viper.Viper, error) {
	var currentConfig *viper.Viper
	viper.BindPFlags(pflag.CommandLine)

	//viper.SetConfigFile(filepath.Join(dir, *configFile))
	viper.SetConfigFile(filename)

	if err := viper.ReadInConfig(); err != nil {
		log.Errorf("Error reading config file, %s", err)
		return currentConfig, errors.New("Error reading config file")
	}

	// Confirm which config file is used
	fmt.Printf("Using config: %s\n", viper.ConfigFileUsed())
	//
	err := godotenv.Load() //берем  ENVIRONMENT  из  файла  или из окружения
	if err != nil {
		log.Error("Error loading .env file")
		return currentConfig, errors.New("Error loading .env file")
	}

	//
	viper.AutomaticEnv() //это  нужно чтобы иметь  доступ к ENV  через viper

	//это  переделать на viper.Get что бы было все единообразно
	log.Info("Environment  from  viper: ", viper.GetString("ENVIRONMENT"))

	//currentEnv := os.Getenv("ENVIRONMENT")
	currentEnv := viper.GetString("ENVIRONMENT")

	if currentEnv == "PROD" {
		currentConfig = viper.Sub("PROD") // выбор  подраздела  конфига
	} else if currentEnv == "DEV" {
		currentConfig = viper.Sub("DEV")
	} else if currentEnv == "TEST" {
		currentConfig = viper.Sub("TEST")
	} else {
		log.Error("Can not get ENVIRONMENT  variable from system")
		log.Error("Please make : export set ENVIRONMENT='PROD' or 'DEV' or 'TEST'")
		return currentConfig, errors.New("Can not get ENVIRONMENT variable from system")

	}
	log.Info("Current env: ", currentEnv)

	if !viper.IsSet(currentEnv + ".host_rabbit_server") {
		log.Error("missing  host_rabbit_server")
		return currentConfig, errors.New("missing host_rabbit_server")
	}

	if !viper.IsSet(currentEnv + ".authkey") {
		log.Error("missing AuthKey")
		return currentConfig, errors.New("missing AuthKey")
	}
	if !viper.IsSet(currentEnv + ".host_db_server") {
		log.Error("missing HostDbServer")
		return currentConfig, errors.New("missing HostDbServer")
	}
	//
	if !viper.IsSet(currentEnv + ".dbname") {
		log.Error("missing DbName")
		return currentConfig, errors.New("missing DbName")
	}
	if !viper.IsSet(currentEnv + ".dbuser") {
		log.Error("missing DbUser")
		return currentConfig, errors.New("missing DbUser")
	}
	if !viper.IsSet(currentEnv + ".dbpassword") {
		log.Error("missing DbPassword")
		return currentConfig, errors.New("missing DbPassword")
	}

	//
	if !viper.IsSet(currentEnv + ".rabbit_user") {
		log.Error("missing rabbit_user")
		return currentConfig, errors.New("missing rabbit_user")
	}
	if !viper.IsSet(currentEnv + ".rabbit_password") {
		log.Error("missing rabbit_password")
		return currentConfig, errors.New("missing rabbit_password")
	}
	if !viper.IsSet(currentEnv + ".sound_file_dir") {
		log.Error("missing sound_file_dir")
		return currentConfig, errors.New("missing sound_file_dir")
	}

	//

	return currentConfig, nil

}
