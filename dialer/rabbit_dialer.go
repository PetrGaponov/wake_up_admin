/*
Диалер который получить задание на звонок из брокера ,  используем rabbitMQ
позвонили, проиграли , возможно запросили dtmf на повторное проигрывание

еще можно предусмотреть обработку  DTMF  на отмену всех последующих звонков на ЭТОТ  конкретный номер
в случае  если совсем все сломалось и будет много звонков.
сделаю так : на ресте будет список с номерами по которым звонить не надо. И ручка от него для добавления / удаления этих номеров
После  dtmf  дергаем эту ручку и заносим туда номер для удаления номера нужно вручную  дернуть другую ручку
TODO:  докладывать о неудачах звонка сразу в Rabbit. параметризировать ttlRedis и delayRepeat . Привести в порядок структуру AnswerToRabbit
Включить время последнего звонка в структуру  Issue и Result
Сделать  отчет об удачном звонке
Придумать как протестить  это все добро
Перенести определения типов в dialer_types.go
*/

package main

import (
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"

	"context"

	"github.com/CyCoreSystems/ari"
	"github.com/CyCoreSystems/ari/client/native"
	"github.com/CyCoreSystems/ari/ext/play"

	"github.com/go-redis/redis"

	"github.com/joho/godotenv"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/streadway/amqp"
)

var debug bool //если true , то рест не трогаем и в базу не пишем ,  просто запускаем  и звоним на номер который передали как первый параметр
var log = logrus.New()

var tr = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
var myClient = &http.Client{Timeout: 2 * time.Second, Transport: tr}
var listIsEmpty = true
var db *sql.DB //наш connection pool
var configurationInst Configuration
var RedisClient *redis.Client //возможно  будем ходить  в redis  из разных Go routine
var maxNumberOfCalls = flag.Int("n", 10, "the number of simultaneously calls")

//var ch, chPub *amqp.Channel //сделаем   пока это глобальныи т к надо иметь доступ из listenApp

//var issuesClientsInst = new(IssuesClients) //это одна на всех конфигурация  норм

//var wg1 sync.WaitGroup
//var waitAllNumbers sync.WaitGroup //это что бы дождаться окончания  всех звонков
//var waitCh = make(chan struct{})  //канал  необходимый для  ожидания звонков

// const generalLogFile = "./dialer.log"
var generalLogFile string

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
	}
}

//Запрос  содержит  только номер кому звоним и список файлов для проигрывания

type JSONTime time.Time

func (t JSONTime) MarshalJSON() ([]byte, error) {
	//do your serializing here
	stamp := fmt.Sprintf("\"%s\"", time.Time(t).Format("2006-01-02 15:04:00 MST"))
	return []byte(stamp), nil
}

type IssuesClients struct { //запрос  на  звонок и PlayBack абоненту либо с генерацией voice файла  либо  с проигрыванием его из параметров запроса
	Issue             string    `json:"issue"` //issue  нам таки нужен Будем генерить  его на Rest автоматом. Он нужен в том числе для организации повторного звонка
	Phone             string    `json:"phone"`
	LastCallTime      time.Time `json:"last_call_time"` //здесь будем  хранить последнее время звонка
	VoiceMessageFiles []string  `json:"files"`          //здесь файлы которые надо проиграть
}

type IssuesBridge struct { //запрос на соединение 2-х абонентов
	Issue        string    `json:"issue"` //issue  нам таки нужен Будем генерить  его на Rest автоматом. Он нужен в том числе для организации повторного звонка
	FirstPhone   string    `json:"first_phone"`
	SecondPhone  string    `json:"second_phone"`
	LastCallTime time.Time `json:"last_call_time"` //здесь будем  хранить последнее время звонка
}

// func (t JSONTime)MarshalJSON() ([]byte, error) {
//     //do your serializing here
//     stamp := fmt.Sprintf("\"%s\"", time.Time(t).Format("Mon Jan _2"))
//     return []byte(stamp), nil
// }

type Configuration struct {
	UserName     string `mapstructure:"username"`
	Password     string `mapstructure:"password"`
	HostAsterisk string `mapstructure:"asterisk_host"`
	Endpoint1    string `mapstructure:"endpoint1"`
	Endpoint2    string `mapstructure:"endpoint2"`
	Timeout      int    `mapstructure:"timeout"`
	Count        int    `mapstructure:"count"`
	Timeout2     int    `mapstructure:"timeout2"`
	//ClientNum []string //здесь массив   номеров
	AuthKey string `mapstructure:"authkey"`
	//HostRestServer   string `mapstructure:"host_rest_server"`
	HostDbServer     string `mapstructure:"host_db_server"`
	HostRabbitServer string `mapstructure:"host_rabbit_server"`
	DbName           int    `mapstructure:"dbname"` //используем  redis поэтому здесь число
	DbUser           string `mapstructure:"dbuser"`
	DbPassword       string `mapstructure:"dbpassword"`
	RabbitUser       string `mapstructure:"rabbit_user"`
	RabbitPassword   string `mapstructure:"rabbit_password"`
	NumberOfRepeats  int    `mapstructure:"number_of_repeats"` //число повторов дозвона в случае неответа
	SoundDir         string `mapstructure:"sound_dir"`         //where is sound files ?
	SoundPrefix      string `mapstructure:"sound_prefix"`      //prefix  for play:  directory  with t2s
	TelPrefix        string `mapstructure:"tel_prefix"`        // prefix  который подставляем при звонке ,  может быть 8 или 7 или
}

// type Answer1 struct {
// 	Message string `json:"message,omitempty"`
// }
type Answer1 struct { //если  нужно будет хранить  какие то другие статусы  добавим их сюда
	SuccessCall bool `json:",omitempty"`
}

type AriClient struct {
	cl             ari.Client     // клиент для подключения
	log            *logrus.Logger //logger
	Issue          string         //issue
	Phone          string         //клиент  будет  создаваться на каждый  звонок свой, и номер клиента будет использоваться для названия APP
	Login          string
	Callid         string
	clientHandler  *ari.ChannelHandle //попробуем хранить handler клиента  в его объекте Это то что вернули из Originate!
	waitTime       time.Duration
	Answer1        string    //это типа ответы на вопросы
	LastCallTime   time.Time `json:"last_call_time"` //здесь будем  хранить последнее время звонка
	NoAnswer       bool
	NumberOfAttemp int
	OriginFile     string
	FilesToPlay    []play.OptionFunc //здесь  сразу  нужно  создать slice с файлами которые нужно проиграть
}

type AnswerToRabbit struct { // Здесь   результат звонка не забыть потом  изменить ее в REST  сервере
	Issue         string    `json:"issue"`
	FirstPhone    string    `json:"first_phone"`
	SecondPhone   string    `json:"second_phone,omitempty"`
	AnswerFirst   Answer1   `json:"answer_first,omitempty"`
	AnswerSecond  Answer1   `json:"answer_second,omitempty"` // будет  пропускаться при конвертации из  JSON в структуру
	LastCallTime  time.Time `json:"last_call_time"`          //здесь будем  хранить последнее время звонка (на Первый номер в случае bridge)
	SuccessDialog bool      `json:"success_dialog"`          //это можно использовать для всего диалога
}

//надо  сюда  прикрутить  еще прометей  мониторинг  и сигналить  о если канал  переполниться !
var answerToRabbitCh = make(chan AnswerToRabbit, 15) //канал  для  связи  с рутиной  которая занимается отправкой данных в RabbitMQ
//var transferToDelay = make(chan IssuesClients, 15)   //для отправки на повторный звонок
var transferToDelay = make(chan IssuesClients, 15) //для отправки на повторный звонок
var guard chan struct{}                            //канал  для ограничения колличества звонков
//Fabric  function
func NewDialer(c Configuration, phone string, waitTime int, filesToPlay []string, issue string) (*AriClient, error) { // создаем новый клиент
	var err error
	currentDial := &AriClient{FilesToPlay: make([]play.OptionFunc, 0)} //создаем текущий звонок и инициализируем на этапе старта создаем сразу  массив  с файлами проигрывания
	currentDial.log = logrus.New()                                     //после создания объекта  нужно будет создать файл с именем phone и перенаправить вывод в него
	currentDial.log.Info("Connecting to ARI")
	currentDial.cl, err = native.Connect(&native.Options{ // юзаем клиента
		Application:  phone, //название app совпадает с тел номером клиента
		Username:     c.UserName,
		Password:     c.Password,
		URL:          "http://" + c.HostAsterisk + ":8088/ari",
		WebsocketURL: "ws://" + c.HostAsterisk + ":8088/ari/events",
	})
	if err != nil {
		currentDial.log.Error("Failed to build ARI client", "error", err)
		return nil, err

	}

	//создание  массива с файлами типа:  play.URI("sound:file")

	//Здесь потом цикл с добавлением  пока просто 1 элемент
	currentDial.OriginFile = filesToPlay[0]                                                                    //нужно для обратной очереди
	currentDial.FilesToPlay = append(currentDial.FilesToPlay, play.URI("sound:"+c.SoundPrefix+filesToPlay[0])) //пока добавим только 1 файл для проигрывания
	currentDial.waitTime = time.Duration(waitTime) * time.Second
	//currentDial.Phone = "8" + phone[len(phone)-10:] //меняем по шаблону
	currentDial.Phone = c.TelPrefix + phone[len(phone)-10:] //меняем по шаблону
	//currentDial.endpointIvr = c.Endpoint2           //через  какой пир звоним на ivr
	fmt.Println("Current dial phone : ", currentDial.Phone)
	currentDial.NoAnswer = true //помечаем  звонок как не отвеченный пока (по умолчанию)
	currentDial.NumberOfAttemp = 0
	currentDial.Issue = issue
	return currentDial, err
}

//
func NewDialerBridge(c Configuration, firstNumber string, secondNumber string, issue string) (*AriClientBridge, error) {
	var err error
	currentDial := &AriClientBridge{}                             //создаем текущий звонок и инициализируем на этапе старта
	currentDial.clientChannel = make(map[string]string)           //соответствие  между клиентским каналом и каналом  ivr
	currentDial.ivrChannel = make(map[string]string)              //MAP  канал IVR - Канал клиента
	currentDial.bridgeHandle = make(map[string]*ari.BridgeHandle) //здесь сохраняем id bridge что бы потом добавить туда канал от IVR.  ключ -  id канала ДО IVR
	currentDial.chanId = make(map[string](chan int))              //  это  каналы  ключи в которых это id channels Клиента  а value  сам channel для связи с handler ом клиента
	currentDial.log = logrus.New()
	currentDial.log.Info("Connecting to ARI")
	currentDial.cl, err = native.Connect(&native.Options{ // юзаем клиента
		Application:  firstNumber + "_bridge",
		Username:     c.UserName,
		Password:     c.Password,
		URL:          "http://" + c.HostAsterisk + ":8088/ari",
		WebsocketURL: "ws://" + c.HostAsterisk + ":8088/ari/events",
	})
	if err != nil {
		currentDial.log.Error("Failed to build ARI client", "error", err)
		return nil, err

	}
	currentDial.endpointIvr = c.Endpoint2 //через  какой пир звоним на ivr
	currentDial.endpoint = c.Endpoint1    // первый пир
	currentDial.firstNumber = firstNumber
	currentDial.secondNumber = secondNumber
	currentDial.issue = issue
	currentDial.NoAnswerPhone1 = true //пока нет ответа
	currentDial.NoAnswerPhone2 = true //пока нет ответа
	currentDial.SuccessDialog = false //пока диалог не состоялся
	return currentDial, err
}

//пока не используется . нужна для очета во внешний rest о результате звонка

func getRequest(url string) (int, error) {

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", configurationInst.AuthKey)
	//r, err := myClient.Get(url)
	r, err := myClient.Do(req)
	if err != nil {
		fmt.Println("Can not connect to REST server")
		log.Error("Can not connect to REST server")
		return 500, err
	}
	if r.StatusCode >= 200 && r.StatusCode <= 299 {
		fmt.Println("HTTP Status is in the 2xx range", r.StatusCode)
		body, err := ioutil.ReadAll(r.Body)
		if err == nil {
			fmt.Println(string(body))
			log.Info("Body answer from rest get request: ", string(body))
		} else {

			log.Info("Error in GetRequest: ", err)
		}
	}
	defer r.Body.Close()

	return r.StatusCode, err

}

//
func IsNumeric(s string) bool {
	val, err := strconv.ParseInt(s, 0, 16)
	if err == nil && val >= 0 && val <= 10 { //проверяем корректность записей
		return err == nil
	} else { //если ошибка парсинга ЛИБО  число не то
		err = errors.New("strconv.ParseInt: parsing invalid syntax")
		fmt.Println(err)
		return err == nil
	}
}

func init() {
	// Init ConfigMap here
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Fatal(err)
	}

	generalLogFile = filepath.Join(dir, "dialer.log")
	log.SetFormatter(&logrus.TextFormatter{
		DisableColors:   false,
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})
}
func main() {

	var conn *amqp.Connection
	var err error

	// using standard library "flag" package
	//Если  noampq  в true  то НЕ используем  rabbitMQ это  для теста  просто звонилки...
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(dir)
	configFile := flag.String("c", "config.json", "config file")

	noAmpq := flag.Bool("noampq", false, "if true no ampq server used. For internal use only")
	flag.Parse()
	fmt.Println("flagname is :", *noAmpq)
	if *maxNumberOfCalls == 0 {
		flag.PrintDefaults()
		os.Exit(1)
	}
	guard = make(chan struct{}, *maxNumberOfCalls)

	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	//pflag.PFlagFromGoFlag(flag.CommandLine)
	pflag.Parse()

	//функция  должна  возвращать объект  pflag
	currentConfig, err := readConfig(filepath.Join(dir, *configFile))
	if err != nil {
		log.Fatal(err)
	}

	//вот здесь  уже  будем  получать  объект   viper и делать Unmarshal  конфигурации
	err = currentConfig.Unmarshal(&configurationInst)
	if err != nil {
		log.Fatalf("unable to decode into struct, %v", err)
	}
	log.Infof("Current config:  %+v", configurationInst)
	fmt.Println(configurationInst.UserName)
	fmt.Println(configurationInst.Password)
	//
	RedisClient = redis.NewClient(&redis.Options{ // киент Redis concurently Safe и может быть использован из разных  go routine
		Addr:         configurationInst.HostDbServer + ":6379",
		Password:     "",                       // no password set
		DB:           configurationInst.DbName, // use default DB
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
	myClient = &http.Client{Timeout: 2 * time.Second}

	//готовим  общий  логгер
	logForCall, err := os.OpenFile(generalLogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	defer logForCall.Close()
	defer db.Close()
	mw := io.MultiWriter(os.Stdout, logForCall)

	if err == nil {
		//log.Out = logForCall
		log.Out = mw
	} else {
		log.Info("Failed to log to file, using default stderr")
		log.Fatalln("Failed to log to file, using default stderr")
	}
	log.Formatter = &logrus.JSONFormatter{}

	for { //внешгий  цикл for
		//	fmt.Println("Connecting...")
		time.Sleep(1 * time.Second)
	Label2:
		for {
			fmt.Println("Connecting RabbitMQ...")
			connectionString := fmt.Sprintf("amqp://%[1]v:%[2]v@%[3]v:5672/", configurationInst.RabbitUser, configurationInst.RabbitPassword, configurationInst.HostRabbitServer)
			fmt.Println(connectionString)
			conn, err = amqp.Dial(connectionString)

			if err != nil {
				log.Println(err)
				time.Sleep(3 * time.Second)
				fmt.Println("Reconnecting  RabbitMQ....")
				continue
			} else {
				break Label2
			}

		}

		//time.Sleep(1 * time.Second)
		fmt.Println("Connected  RabbitMQ!!!")
		rabbitCloseError := make(chan *amqp.Error)
		notify := conn.NotifyClose(rabbitCloseError) //error channel
		//failOnError(err, "Failed to connect to RabbitMQ")
		//defer conn.Close()

		ch, err := conn.Channel() //channel for consumer
		failOnError(err, "Failed to open a channel")
		chPub, err := conn.Channel() //channel for Publisher
		failOnError(err, "Failed to open a channel chPub")
		err = chPub.ExchangeDeclare(
			"dialerInput", // name
			"direct",      // type
			true,          // durable
			false,         // auto-deleted
			false,         // internal
			false,         // no-wait
			nil,           // arguments
		)
		failOnError(err, "Failed to declare  exchange")

		err = ch.Qos( //Qos  for consumer
			1,     // prefetch count
			0,     // prefetch size
			false, // global
		)
		failOnError(err, "Failed to set QoS")

		defer ch.Close()
		//отдельная go rounine  для отправки в Rabbit
		//запускаем эту go routine для отправки сообщений в RabbitMq
		go func() { //  если получили  ошибку  нужно перезапустить эту GO-Routine!!!
			//здесь нужна еще одна очередь , что бы доложить о звонке типа Bridge В resultsDialer ждет

			q, err := chPub.QueueDeclare(
				"resultsDialer", // name
				true,            // durable
				false,           // delete when unused
				false,           // exclusive
				false,           // no-wait
				nil,             // arguments
			)
			//добавил объявление  очереди т к ссылаемся на нее раньше чем она объявлена!!!!
			ch.QueueDeclare( //queue   declare for CONSUMER!!
				"dialer", // name
				true,     // durable
				false,    // delete when unused
				false,    // exclusive
				false,    // no-wait
				nil,      // arguments
			)
			//
			failOnError(err, "Failed to open a channel chPub")
			fmt.Println(" Result  Name: ", q.Name)

			argsDelay := make(amqp.Table)
			argsDelay["x-message-ttl"] = int32(60000) //60 с время в миллисекундах  это параметризировать
			argsDelay["x-dead-letter-exchange"] = "dialerInput"
			argsDelay["x-dead-letter-routing-key"] = "dialer"

			qDelay, err := chPub.QueueDeclare(
				"delayDialer", // name
				true,          // durable
				false,         // delete when unused
				false,         // exclusive
				false,         // no-wait
				argsDelay,     // arguments
			)
			fmt.Println(" Delay Name: ", qDelay.Name)
			failOnError(err, "Failed to open a channel qDelay")
			//BIND   делается над queue Consumer
			//добавил привязку dialer очереди
			err = ch.QueueBind("dialer", "dialer", "dialerInput", false, nil) //привязываем очередь  результатов к Exchange point
			failOnError(err, "Failed to bind  queue dialer")
			//err = ch.QueueBind("resultsDialer", "resultsDialer", "dialerInput", false, nil) //привязываем очередь  результатов к Exchange point
			//failOnError(err, "Failed to bind  queue resultsDialer")
			err = ch.QueueBind("delayDialer", "delayDialer", "dialerInput", false, nil) //привязываем очередь  delay к Exchange point
			failOnError(err, "Failed to bind  queue delayDialer")

			//
			for {
				select {
				case message := <-answerToRabbitCh:
					body, err := json.Marshal(message)
					if err != nil {
						log.Info(err)
					}

					err = chPub.Publish(
						"dialerInput",   // exchange
						"resultsDialer", // routing key  Здесь
						false,           // mandatory
						false,           // immediate
						amqp.Publishing{
							ContentType: "application/json",
							Body:        []byte(body),
						})

					if err != nil {
						log.Info(err)
					}
					fmt.Println("In  ANSWER to Rabbit channel : ", message)

				case messageToDelay := <-transferToDelay:
					body, err := json.Marshal(messageToDelay)
					if err != nil {
						log.Info(err)
					}

					err = chPub.Publish(
						"dialerInput", // exchange
						"delayDialer", // routing key
						false,         // mandatory
						false,         // immediate
						amqp.Publishing{
							ContentType: "application/json",
							Body:        []byte(body),
						})

					if err != nil {
						log.Info(err)
					}
					fmt.Println("In DELAY channel: ", messageToDelay)
					//вот здесь добавил код обработки ошибки
				case err = <-notify:
					return //выход из коротины

				}
			}
			//
			//здесь получаем  готовую структуру  для ответа в rabbitMQ
			//}  //конец for range

		}()
		//здесь  нужно  определить  еще одну очередь  для звонков с  bridge
		qBridge, err := ch.QueueDeclare( //queue   declare for CONSUMER!!
			"dialerBridge", // name
			true,           // durable
			false,          // delete when unused
			false,          // exclusive
			false,          // no-wait
			nil,            // arguments
		)
		failOnError(err, "Failed to declare a queue")
		fmt.Println(" Input Name: ", qBridge.Name)
		err = ch.QueueBind("dialerBridge", "dialerBridge", "dialerInput", false, nil) //привязываем основную очередь  к Exchange point
		failOnError(err, "Failed to bind  queue dialerBridge")
		msgsBridge, err := ch.Consume( //consumer messages
			qBridge.Name, // queue
			"",           // consumer
			false,        // auto-ack  //Внимание вот здесь надо сделать ручное подтверждение!!!
			false,        // exclusive
			false,        // no-local
			false,        // no-wait
			nil,          // args
		)
		failOnError(err, "Failed to register a consumer")

		//
		q, err := ch.QueueDeclare( //queue   declare for CONSUMER!!
			"dialer", // name
			true,     // durable
			false,    // delete when unused
			false,    // exclusive
			false,    // no-wait
			nil,      // arguments
		)
		failOnError(err, "Failed to declare a queue")
		fmt.Println(" Input Name: ", q.Name)
		err = ch.QueueBind("dialer", "dialer", "dialerInput", false, nil) //привязываем основную очередь  к Exchange point
		failOnError(err, "Failed to bind  queue dialer")
		msgs, err := ch.Consume( //consumer messages
			q.Name, // queue
			"",     // consumer
			false,  // auto-ack  //Внимание вот здесь надо сделать ручное подтверждение!!!
			false,  // exclusive
			false,  // no-local
			false,  // no-wait
			nil,    // args
		)
		failOnError(err, "Failed to register a consumer")
		var d amqp.Delivery
		var dBridge amqp.Delivery
		//	go func() {
	Label:
		for { //внутренний цикл
			select { //check connection
			case err = <-notify:
				//work with error

				fmt.Println("LOST RabbitMQ  CONNECTION !!!")
				time.Sleep(1 * time.Second)
				break Label //reconnect
			case d = <-msgs: //вот  здесь  надо процессить  сообщения но делать это надо в отдельной go routine
				//Здесь   получаем сообщение  из очереди
				var issuesClientsInst = new(IssuesClients) //создаем конфиг по issue

				fmt.Printf("%T", q)
				fmt.Printf("%T", msgs)
				log.Printf("Received a message: %s", d.Body)
				err := json.Unmarshal([]byte(d.Body), &issuesClientsInst)
				if err != nil { //если не смогли  распарсить сообщение, подтверждаем и уходим на след итерацию
					fmt.Println("Error:", err)
					log.Fatalf("%s: %s", d.Body, err)
					d.Ack(false) //подтверждаем ТОЛЬКО текщее сообщение  вручную
					continue
				}
				//if _, err := os.Stat(configurationInst.SoundDir + issuesClientsInst.VoiceMessageFiles[0] + ".wav"); os.IsNotExist(err) {
				if _, err := os.Stat(filepath.Join(configurationInst.SoundDir, issuesClientsInst.VoiceMessageFiles[0]+".wav")); os.IsNotExist(err) {
					//fmt.Println("file not exist") //если файла который на надо проиграть нет, то подтверждаем текущее сообщение и берем след сообщение из очереди
					log.Error("file not exist") //если файла который на надо проиграть нет, то подтверждаем текущее сообщение и берем след сообщение из очереди
					d.Ack(false)                //подтверждаем ТОЛЬКО текщее сообщение  вручную
					continue
					// path/to/whatever exists
				}
				//Далее ограничитель на колличество ОДНОВРЕМЕННЫХ звонков
				//здесь  заблокируемся и теоретически  будем ждать
				guard <- struct{}{} // Ограничение колличества одновременных звонков!!!
				fmt.Println("После guard!!!")
				d.Ack(false) // Подтвердили  ТОЛЬКО текщее сообщение  вручную сей час будем звонить...

				//далее в issuesClientsInst имеем то куда нам звонить нужно
				//запускаем рутину  для создания  звонка. Именно так, что бы не блокировать цикл  обработки  событий
				//Используем  анонимную  рутину т к нужна  объемлеющая  область видимости
				go func(phoneNum string, filesToPlay []string, issue string) { //рутина в которой звоним
					//передаеем  в рутину номер куда звоним , issue и callid (id нужен потом для идентификации этой обработки)

					var h *ari.ChannelHandle
					var waitAllNumbers sync.WaitGroup //это что бы дождаться окончания  звонка и его обработки
					waitAllNumbers.Add(1)             //у нас 1 звонок

					//здесь все остальное
					phone := strings.TrimSpace(phoneNum)
					//issue = strings.TrimSpace(issue)
					caller, err := NewDialer(configurationInst, phoneNum, 10, filesToPlay, issue)
					if err != nil {
						fmt.Errorf("filed to create object Ari")
						waitAllNumbers.Done() //минусуем
						return
					}
					//далее настройки логгера
					logForCall, err := os.OpenFile(dir+"/"+phone+".log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
					defer logForCall.Close()
					mw := io.MultiWriter(os.Stdout, logForCall)
					if err == nil {
						//log.Out = logForCall
						caller.log.Out = mw
					} else {
						caller.log.Info("Failed to log to file, using default stderr")

					}
					caller.log.Formatter = &logrus.JSONFormatter{}
					// конец настроек логгера
					fmt.Println(phone)
					log.Info("Connecting to ARI")

					log.Info("Starting listener app")
					//Кадый звонок клиенту, это отдельное ARI подключение. Нужно создавать множество go Routines  по колличеству звогков

					ctx, _ := context.WithCancel(context.Background())
					//ctx2, cancel2 := context.WithCancel(context.Background()) //временно

					go caller.listenApp(ctx, &waitAllNumbers, caller.cl, caller.channelHandler) //Здесь  chan Handle передается как параметр  Это клиент который ловит события !!!!!

					log.Info("Starting Listener")

					//number := phone
					//number = strings.TrimSpace(number)
					log.Info("Start call: ", phone)

					h, err = caller.createCall(caller.cl, phone, configurationInst.Endpoint1)

					if err != nil {
						log.Error("Failed to create call", "error", err)
						caller.cl.Close()
						waitAllNumbers.Done()
						//здесь посмореть  повнимательней, возможно одного return мало
						return
					} else {
						//d.Ack(false) //подтверждаем вручную Потом нужно сделать подтверждение вручную
						fmt.Println("Подтвердили ...")
					}
					caller.clientHandler = h //  Сохраняем это пока, можем потом сматчить !

					fmt.Println(caller.clientHandler.ID()) //вот этот идентификатор и можем использовать для эмуляции в rest NAUMEN

					go func() { //для теста  посмотрим на cause code
						h_request := h.Subscribe(ari.Events.ChannelDestroyed)
						defer h_request.Cancel()

						for {
							select {
							case e, ok := <-h_request.Events():
								if !ok {
									log.Error("channel subscription ChannelHangupRequest closed")
									return
								}
								v := e.(*ari.ChannelDestroyed)
								log.Info("Cause code: ", v.Cause)

							}
						}

					}()
					//
					waitAllNumbers.Wait() //ждем пока не обработаем этот звонок
					caller.cl.Close()
					//cancel2()
					<-guard //  На выходе считываем что бы освободить место для следующего message

				}(configurationInst.TelPrefix+issuesClientsInst.Phone[len(issuesClientsInst.Phone)-10:], issuesClientsInst.VoiceMessageFiles, issuesClientsInst.Issue) //передаем  телефон и список файлов которые надо проиграть
			//конец  кода  звонилки
			case dBridge = <-msgsBridge: //здесь   получаем  сообщения  из канала для создания bridge
				//Здесь   получаем сообщение  из очереди
				var issuesClientsInstBridge = new(IssuesBridge) //создаем конфиг по issue

				fmt.Printf("%T", qBridge)
				fmt.Printf("%T", msgsBridge)
				log.Printf("Received a message: %s", dBridge.Body)
				err := json.Unmarshal([]byte(dBridge.Body), &issuesClientsInstBridge)
				if err != nil {
					fmt.Println("Error:", err)
					log.Fatalf("%s: %s", dBridge.Body, err)
					dBridge.Ack(false) //подтверждаем ТОЛЬКО текщее сообщение  вручную
					continue
				}
				guard <- struct{}{} // would block if guard channel is already filled
				fmt.Println("После guard!!!")
				dBridge.Ack(false) //подтверждаем ТОЛЬКО текщее сообщение  вручную

				//модифицированная  рутина для создания звонка
				go func(firstPhone string, secondPhone string, issue string) { //рутина в которой звоним
					//передаеем  в рутину номер куда звоним , issue и callid (id нужен потом для идентификации этой обработки)

					var h *ari.ChannelHandle
					var waitAllNumbers sync.WaitGroup //это что бы дождаться окончания  звонка и его обработки
					waitAllNumbers.Add(1)             //у нас 1 звонок

					caller, err := NewDialerBridge(configurationInst, firstPhone, secondPhone, issue)
					if err != nil {
						fmt.Errorf("filed to create object Ari")
						waitAllNumbers.Done() //минусуем
						return
					}
					//далее настройки логгера  делаем  настройки логгера по первому  телефону
					logForCall, err := os.OpenFile(dir+"/"+firstPhone+"_bridge"+".log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
					defer logForCall.Close()
					mw := io.MultiWriter(os.Stdout, logForCall)
					if err == nil {
						//log.Out = logForCall
						caller.log.Out = mw
					} else {
						caller.log.Info("Failed to log to file, using default stderr")

					}
					caller.log.Formatter = &logrus.JSONFormatter{}
					// конец настроек логгера
					fmt.Println(firstPhone)
					log.Info("Connecting to ARI")

					log.Info("Starting listener app")
					//Кадый звонок клиенту, это отдельное ARI подключение. Нужно создавать множество go Routines  по колличеству звогков

					ctx, cancel := context.WithCancel(context.Background())

					go caller.listenAppBridge(ctx, caller.cl, caller.channelHandlerBridge) //Здесь  chan Handle передается как параметр  Это клиент который ловит события !!!!!

					log.Info("Starting Listener")

					//звонок  первый притендент на то чтобы стать  интерфейсом  Переделать !
					h, err = caller.createCallBridge(caller.firstNumber, caller.endpointIvr) //звоним

					if err != nil {
						log.Error("Failed to create call", "error", err)
						caller.cl.Close()
						waitAllNumbers.Done() // минусуем звонок
						//здесь посмореть  повнимательней, возможно просто return не достаточно
						return
					} else {
						//dBridge.Ack(false)   //может  лучше  здесь подтверждать ?
						fmt.Println("Подтвердили ...")
					}

					caller.clientChannel[h.ID()] = "" //соответствие  между клиентским каналом и каналом  ivr
					caller.log.Info("Channel id  Client  from originate is:  ", h.ID())
					caller.chanId[h.ID()] = make(chan int)         // ДЛя  связи между GO Routines Если будет  Asterisk 14 и расширенных статусов handler запустится полюбому. Надо будет его убивать по таймауту
					dialtimeout := time.NewTimer(time.Second * 30) //30 с на дозвон на оба номера
					for {
						caller.log.Info("FOR!")
						select { //здесь блокируемся  пока что то не придет в наш канал
						case <-caller.chanId[h.ID()]: //
							//case <-exitChan:
							//вот здесь  и нужно сделать  отчет в rabbit об успешности звонка
							toRabbit := new(AnswerToRabbit)
							toRabbit.Issue = caller.issue
							toRabbit.FirstPhone = caller.firstNumber
							toRabbit.SecondPhone = caller.secondNumber
							//здесь создаем 2 структуры
							// answerJson := fmt.Sprintf(`{"success_call": %t}`, caller.NoAnswerPhone1) //
							data1 := &Answer1{}
							data1.SuccessCall = caller.NoAnswerPhone1
							// err := json.Unmarshal([]byte(answerJson), data1)
							// if err != nil {
							// 	fmt.Println(err)
							// }
							toRabbit.AnswerFirst = *data1
							// answerJson = fmt.Sprintf(`{"success_call": %t}`, caller.NoAnswerPhone2) //
							data2 := &Answer1{}
							data2.SuccessCall = caller.NoAnswerPhone2
							// err = json.Unmarshal([]byte(answerJson), data2)
							// if err != nil {
							// 	fmt.Println(err)
							// }
							toRabbit.AnswerSecond = *data2
							toRabbit.SuccessDialog = caller.SuccessDialog
							toRabbit.LastCallTime = caller.LastCallTime
							//fmt.Println("Data answer1  ")

							answerToRabbitCh <- *toRabbit

							caller.log.Info("Пришло событие в канал. Пора сворачиваться !!!!")
							//h.Hangup()                  //
							time.Sleep(2 * time.Second)   //подождем пока остальные завершат работу.
							delete(caller.chanId, h.ID()) //удаляем после себя  map с каналом
							//здесь надо бы sync.WaitGroup
							return //выходим

						case <-dialtimeout.C: //30  c на дозвон  timeout потом выключаемся  Переделать это по человечески!

							caller.log.Info("Событие в dialtimeout")

							if len(caller.clientChannel[h.ID()]) < 1 { //Но если в этой map есть строка длиной > 1 (id канала), то значит уже дозвонились,и таймер не нужен
								caller.log.Info("Зашли в проверку")

								h.StopRing()
								h.Hangup()

								caller.log.Info("не дозвонились")
								time.Sleep(2 * time.Second)          //подождем пока остальные завершат работу.
								delete(caller.chanId, h.ID())        //удаляем КАНАЛ  в map
								delete(caller.clientChannel, h.ID()) //удаляем запись о Channel ID звонка клиенту
								<-guard                              //  читаем  из канала что бы дать очередь другому звонку
								cancel()
								caller.cl.Close()
								return
							} // else

						}
					}

				}("7"+issuesClientsInstBridge.FirstPhone[len(issuesClientsInstBridge.FirstPhone)-10:], "7"+issuesClientsInstBridge.SecondPhone[len(issuesClientsInstBridge.SecondPhone)-10:], issuesClientsInstBridge.Issue) //передаем  телефон и список файлов которые надо проиграть

				//конец  фуекции для создания звонка в Bridge
			} //конец  select
		} //конец внутреннего цикла
	} //конец  внешнего цикла
} //  конец  main   функции

// далее функции которые используются для  создания и обработки  звонка
func (a *AriClient) listenApp(ctx context.Context, waitAllNumbers *sync.WaitGroup, cl ari.Client, handler func(ctx context.Context, cl ari.Client, h *ari.ChannelHandle)) { //добавил ctx
	//func (a *AriClient) listenApp(ctx context.Context, cl ari.Client, handler func(ctx context.Context, h *ari.ChannelHandle))
	//a.clientHandler
	//уйдем отсюда  либо по таймауту  либо  после окончания звонка в StasisStop

	sub := cl.Bus().Subscribe(nil, "StasisStart")
	end := cl.Bus().Subscribe(nil, "StasisEnd")
	var current_handler *ari.ChannelHandle
	timeout := time.Duration(configurationInst.Timeout) * time.Second //это таймаут  для не отвеченного звонка
	timeoutTimer := time.NewTimer(timeout)                            // это таймер который мы будем переустанавливать

	for {
		select {
		case e := <-sub.Events(): //Если  пришло  событие Stasis Start
			v := e.(*ari.StasisStart)
			log.Info("Got stasis start", "channel", v.Channel.ID)
			current_handler = cl.Channel().Get(v.Key(ari.ChannelKey, v.Channel.ID))    //если был stasis start то будет и stasis end
			timeoutTimer.Stop()                                                        //таймер уже не нужен
			go handler(ctx, cl, cl.Channel().Get(v.Key(ari.ChannelKey, v.Channel.ID))) //Запускаем handler НО можем *ari.ChannelHandle взять из a.clientHandler
		case <-end.Events(): //если  здесь  побывали  значит  таки stasis start был и звонок успешный
			//в end  event  можно обработать отмену

			time.Sleep(3 * time.Second) //
			a.log.Info("Got stasis end")
			fmt.Println("Got stasis end")
			//wg1.Done()
			fmt.Println(a.NoAnswer)
			if a.NoAnswer == true {
				fmt.Println("Client with number: ", a.Phone, " reset call!")
			}
			if a.Answer1 == "" || (a.Answer1 != "" && !IsNumeric(a.Answer1)) { //нажал не число (* или # например)
				fmt.Println("DTMF  события небыло ")

			} else {
				//var id int
				//далее из ответа  делаем json для  записи его в базу
				//
				fmt.Println("Была  обработка DTMF")

			}

			data := &Answer1{}

			data.SuccessCall = true

			toRabbit := new(AnswerToRabbit)
			toRabbit.Issue = a.Issue
			toRabbit.FirstPhone = a.Phone
			toRabbit.AnswerFirst = *data
			toRabbit.SuccessDialog = true
			toRabbit.LastCallTime = a.LastCallTime
			fmt.Println("Success answer !!!: ", data.SuccessCall)

			answerToRabbitCh <- *toRabbit

			//
			waitAllNumbers.Done() //сигнализируем что один звонок закончен

			current_handler.Hangup() // поменять

			return
			//Вот здесь  будет косяк, если использовать это для перевода на опера, и длительных  разговоров , то этого таймера будет мало, подумать как лучше!
		case <-timeoutTimer.C: //ага  тушим таймер заранее
			//Еще  думаю что стоит  докладывать в результаты о каждой попытке  дозвона , а не токлько  о суммарных НЕ успешных
			//case <-time.After(time.Duration(configurationInst.Timeout) * time.Second): //20  c на ВСЕ, звонок и разговор .звонок timeout потом выключаемся
			//Здесь нужно проверять, есть ли answer или нет . если есть, то возможно это переведенный звонок  и делать ничего не нужно
			if !a.NoAnswer { //если  звонок уже отвеченный , то не нужно уходить по таймауту
				timeoutTimer.C = nil //либо переустанавливаем таймаут  , либо исключаем эту ветку ставя  канал в nil
				// timeoutTimer.Reset(40 * time.Minute) //переустанавливаем  таймер на длительное время если звонок отвечен
				continue //продолжаем ловить  события в

			} else {
				//давай так, если не отвеченный смотрим в редис по issue и берем число  дозвонов если оно >=2 то нафиг такой звонок, докладываемся о недозвоне
				// иначе  стави  1  или увеличиваем число  попыток , пишем это в очередь на повторный звонок
				//здесь  нужно  проверить  какая попытка дозвона  идет и решить куда посылаем звонок
				numAttemp, err := RedisClient.Get(a.Issue).Result() // число  попыток или  ошибка
				fmt.Println("Текущее число попыток дозвона из Redis: ", numAttemp)
				fmt.Println(" ")
				if err != nil { //если здесь ошибка , то записи в редисе нет устанавливаем в 1        // || (err==nil && strconv.Atoi(numAttemp) < 2) { //если  запись по этому issue не  существует еще ИЛИ есть в redis  и колличество дозвонов меньше двух то идем на
					//теперь номер  попытки  должен  присутствовать  либо в структуре  либо  нужно писать его в редис
					res := RedisClient.Set(a.Issue, 1, 6*time.Minute).Err() //параметризировать TTL  что бы не засорять БД
					if res != nil {
						log.Info(res, " Filed to set redis  number  of attemp!!!!!")
						waitAllNumbers.Done()
						return
					} else {
						fmt.Println("Первый раз отправляем в повторную очередь ")

					}

					delayInst := &IssuesClients{VoiceMessageFiles: make([]string, 1)} //здесь наверное не issue надо использовать а
					//или new(IssuesClients)
					delayInst.Issue = a.Issue
					delayInst.Phone = a.Phone
					delayInst.VoiceMessageFiles[0] = a.OriginFile
					//delayInst.Login = a.Login
					//delayInst.Callid = a.Callid

					transferToDelay <- *delayInst

				} else if numAttempInt, _ := strconv.Atoi(numAttemp); numAttempInt < configurationInst.NumberOfRepeats { //здесь точно err == nil, и проверяем что колличество попыток < 2 (по умолчанию)
					//res := RedisClient.SetXX(a.Issue, numAttempInt+1, 15*time.Minute)
					_, res := RedisClient.Incr(a.Issue).Result() //увеличиваем  счетчик
					if res != nil {
						log.Info(res, " Filed to set redis  number  of attemp +1!")
						waitAllNumbers.Done()
						return
					}
					fmt.Println("Заинкрементили значение колличества  попыток  дозвона")
					//здесь  тоже  отправляем в transferToDelay
					delayInst := &IssuesClients{VoiceMessageFiles: make([]string, 1)} //здесь наверное не issue надо использовать а
					//или new(IssuesClients)
					delayInst.Issue = a.Issue
					delayInst.Phone = a.Phone
					delayInst.VoiceMessageFiles[0] = a.OriginFile
					transferToDelay <- *delayInst

				} else {
					fmt.Println("Просто  отправляем  в результат  что 2 раза не дозвонились ")

					data := &Answer1{}
					data.SuccessCall = false

					//здесь тоже нужно отчитаться что сбросили звонок
					toRabbit := new(AnswerToRabbit)
					toRabbit.Issue = a.Issue
					toRabbit.FirstPhone = a.Phone
					toRabbit.LastCallTime = a.LastCallTime
					//toRabbit.Login = a.Login
					//toRabbit.Callid = a.Callid
					toRabbit.AnswerFirst = *data
					toRabbit.SuccessDialog = false

					answerToRabbitCh <- *toRabbit
				}
				fmt.Println("Exit after timeout in listenApp") // вдруг трубку не взяли и соответственно небыло hangup
				log.Info("Exit after timeout in listenApp")

				if current_handler != nil { //если есть хендлер значит был stasis start
					current_handler.StopRing() //
					current_handler.Hangup()   //
					waitAllNumbers.Done()      //минус один в канал
				} else {
					waitAllNumbers.Done()
				}

				return

				//конец  последнего  case
			} //добавил
		}
	}

}

func (a *AriClient) createCall(cl ari.Client, number string, endpoint string) (h *ari.ChannelHandle, err error) {

	h, err = a.cl.Channel().Originate(nil, ari.OriginateRequest{
		Endpoint: endpoint + number,
		App:      a.Phone,
		Timeout:  45, // 15 сек на дозвон
		//CallerID: caller,
	})
	fmt.Println("Phone in createcall: ", a.Phone)
	//Здесь  регистрируем время звонка

	a.NumberOfAttemp += 1 //делаем инкремент колличества попыток дозвона
	//
	loc, _ := time.LoadLocation("Europe/Moscow")
	//var t time.Time
	now := time.Now()
	//t=time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), now.Nanosecond(),loc)
	//  устанавливаем  время начала звонка
	a.LastCallTime = time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), loc)
	//a.LastCallTime = JSONTime(time.Now())
	//

	return
}

func (a *AriClient) channelHandler(ctx context.Context, scl ari.Client, h *ari.ChannelHandle) { //добавил context
	//Этот  handler  вызывается   при  поднятии канала полюбасу
	a.log.Info("Running channel handler")
	a.NoAnswer = false
	fmt.Println("Channel handle from param: ", h.ID())
	fmt.Println("Channel handle from object: ", a.clientHandler.ID())

	stateChange := h.Subscribe(ari.Events.ChannelStateChange)
	defer stateChange.Cancel()

	data, err := h.Data()
	a.log.Info("All data1: ", data)
	if err != nil {
		log.Error("Error getting data", "error", err)
		return
	}
	a.log.Info("Channel State ", "state: ", data.State)
	a.log.Info("all data: ", data)

	var wg sync.WaitGroup //waitGroup  для ожидания рутины проигрывания  сообщения и обработки dtmf

	wg.Add(1)
	go func() {
		a.log.Info("Waiting for channel events")
		a.log.Info("Handler RUN. ")
		defer wg.Done() // разблокируемся при выходе
		//timer := time.NewTimer(time.Second * 7)
		data, err = h.Data()
		if err != nil {
			log.Error("Error getting data", "error", err)
			return
		}

		a.log.Info("Current state in go routine: ", "state ", data.State)
		caller_number := data.GetCaller()
		a.log.Info(caller_number.GetNumber()) //получаем callerid из канала
		//вот это вот цикл с повтором  вместо  конкретного sound  можно передавать  SLICE   из  OptionFunc  он  распакуется т к -> opts ...OptionFunc
		//	for { //начало цикла     сигнатура у Play(ctx context.Context, p ari.Player, opts ...OptionFunc) сюда можно slice впендюрить

		//if err := play.Play(ctx, h, play.URI("sound:attention"), play.Replays(1)).Err(); err != nil {
		if err := play.Play(ctx, h, a.FilesToPlay[0], play.Replays(1)).Err(); err != nil {
			log.Error("failed to play sound", "error", err)
			return
		}
		a.log.Info("completed playback at ALL")
		//Далее обработаем повтор проигрывания , может  спящий  админ не расслышал что сломалось
		//Обработку  DTMF  пока закомментил
		// res := play.Prompt(ctx, h, play.URI("sound:press_any_key_to_replay"), play.Replays(0), play.DigitTimeouts(a.waitTime, 3*time.Second, a.waitTime)) //это нужно параметризировать

		// if err := res.Err(); err != nil {
		// 	log.Error("failed to play sound", "error", err)
		// 	return
		// }
		// res2, _ := res.Result()
		// a.log.Info("Pressed: ", res2.DTMF)
		// //fmt.Println(reflect.TypeOf(res2.DTMF))
		// a.Answer1 = res2.DTMF //сохраняем ответ в клиенте

		// a.log.Info("DTMF play end success")
		// a.log.Info("Abonent press key : ", res2.DTMF)
		// //возможно стоит перенести все это в StasisEnd
		// if res2.DTMF != "" && IsNumeric(res2.DTMF) { //если что то нажали И это нужная нам цифра

		// 	fmt.Println("получили правильный DTMF")
		// 	continue //если правильный  dtmf  повторим  проигрывание  сообщения
		// 	//ch := make(chan int, 1)
		// 	//requestString := fmt.Sprintf("%s/rest/scriptrunner/latest/custom/setivr?issue=%s&answer=a1:%s", configurationInst.HostRestServer, a.Issue, res2.DTMF)
		// 	//a.log.Info("Request string : ", requestString)
		// 	//асинхронно делаем запрос
		// 	//go MakeRequest(requestString, ch) //здесь запишем в канал

		// 	// if err := play.Play(ctx, h, play.URI("sound:good_by"), play.URI("sound:service/sound")).Err(); err != nil {
		// 	// 	log.Error("failed to play sound", "error", err)
		// 	// 	return
		// 	// }
		// 	// для проекта  с оповещение  надо  наоборот  сначала play  , потом DTMF  и  организовать повтор

		// 	//a.log.Info("Response from channel: ", <-ch) //здесь прочитаем из канала

		// } // } else if res2.DTMF != "" && !IsNumeric(res2.DTMF) { //клиент нажал что то не то
		// 	if err := play.Play(ctx, h, play.URI("sound:good_by")).Err(); err != nil {
		// 		log.Error("failed to play sound", "error", err)
		// 		return
		// 	}
		// 	a.log.Info("Некорректные данные в dtmf")
		// 	break
		// }
		//} //конец цикла  повтора

		a.clientHandler.Hangup() //потушить звонок
		//далее пока убрал  код  который   нужен для использования не originate  а dial

	}()

	//h.Answer()

	wg.Wait()

	h.Hangup()
}

func readConfig(filename string) (*viper.Viper, error) { //функция  для чтения конфига принимает путь до конфига в виде строки
	var currentConfig *viper.Viper //
	viper.BindPFlags(pflag.CommandLine)

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
		//os.Exit(1)
	}
	log.Info("Current env: ", currentEnv)

	// check  if all fields provided
	if !viper.IsSet(currentEnv + ".UserName") {
		log.Error("missing UserName")
		return currentConfig, errors.New("missing UserName")
	}
	if !viper.IsSet(currentEnv + ".Password") {
		log.Error("missing Password")
		return currentConfig, errors.New("missing Password")
	}
	if !viper.IsSet(currentEnv + ".asterisk_host") {
		log.Error("missing HostAsterisk")
		return currentConfig, errors.New("missing HostAsterisk")
	}
	if !viper.IsSet(currentEnv + ".Endpoint1") { //
		log.Error("missing Endpoint1")
		return currentConfig, errors.New("missing Endpoint1")
	}
	if !viper.IsSet(currentEnv + ".Endpoint2") {
		log.Error("missing Endpoint2")
		return currentConfig, errors.New("missing Endpoint2")
	}
	if !viper.IsSet(currentEnv + ".Timeout") {
		log.Error("missing Timeout")
		return currentConfig, errors.New("missing Timeout")
	}
	if !viper.IsSet(currentEnv + ".Count") {
		log.Error("missing Count")
		return currentConfig, errors.New("missing Count")
	}
	if !viper.IsSet(currentEnv + ".Timeout2") {
		log.Error("missing Timeout2")
		return currentConfig, errors.New("missing Timeout2")
	}
	if !viper.IsSet(currentEnv + ".AuthKey") {
		log.Error("missing AuthKey")
		return currentConfig, errors.New("missing AuthKey")
	}
	// if !viper.IsSet(currentEnv + ".host_rest_server") {
	// 	log.Error("missing HostRestServer")
	// 	return currentConfig, errors.New("missing host_rest_server")
	// }
	if !viper.IsSet(currentEnv + ".host_db_server") {
		log.Error("missing HostDbServer")
		return currentConfig, errors.New("missing host_db_server")
	}
	if !viper.IsSet(currentEnv + ".DbName") {
		log.Error("missing DbName")
		return currentConfig, errors.New("missing DbName")
	}
	if !viper.IsSet(currentEnv + ".DbUser") {
		log.Error("missing DbUser")
		return currentConfig, errors.New("missing DbUser")
	}
	if !viper.IsSet(currentEnv + ".DbPassword") {
		log.Error("missing DbPassword")
		return currentConfig, errors.New("missing DbPassword")
	}
	//
	if !viper.IsSet(currentEnv + ".rabbit_user") {
		log.Error("missing RabbitUser")
		return currentConfig, errors.New("missing rabbit_user")
	}
	if !viper.IsSet(currentEnv + ".rabbit_password") {
		log.Fatal("missing RabbitPassword")
		return currentConfig, errors.New("missing rabbit_password")
	}
	if !viper.IsSet(currentEnv + ".number_of_repeats") {
		log.Error("missing NumberOfRepeats")
		return currentConfig, errors.New("missing number_of_repeats")
	}
	if !viper.IsSet(currentEnv + ".sound_dir") {
		log.Error("missing SoundDir")
		return currentConfig, errors.New("missing sound_dir")
	}
	if !viper.IsSet(currentEnv + ".sound_prefix") {
		log.Error("missing SoundPrefix")
		return currentConfig, errors.New("missing sound_prefix")
	}
	if !viper.IsSet(currentEnv + ".tel_prefix") {
		log.Error("missing TelPrefix")
		return currentConfig, errors.New("missing tel_prefix")
	}
	//

	return currentConfig, nil

}
