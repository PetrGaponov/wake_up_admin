package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"net/url"
	"reflect"
	"strconv"
	"unicode"

	"encoding/base64"
	"encoding/binary"
	"encoding/json" //попробовать  заменить  стандартный   пакет json на  https://github.com/json-iterator/go
	"errors"

	//"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	//jsoniter "github.com/json-iterator/go"

	"github.com/Sirupsen/logrus"
	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/go-redis/redis"
	"github.com/joho/godotenv"

	//golang_tts "github.com/leprosus/golang-tts"
	golang_tts "github.com/PetrGaponov/golang-tts"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/streadway/amqp"
)

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
	}
}

func fib(n int) int {
	if n == 0 {
		return 0
	} else if n == 1 {
		return 1
	} else {
		return fib(n-1) + fib(n-2)
	}
}

type RequestFile struct { //структура  для запроса создания файла
	Issue   string `json:"issue"`   // по этому issue сгенерим назв файла
	Message string `json:"message"` //здесь сообщение которое нужно озвучить
}

type Configuration struct { //наша  конфигурация  потом  добавятся и key для yandex speech
	HostRabbitServer string `mapstructure:"host_rabbit_server"`
	AccessKeyPolly   string `mapstructure:"access_key_polly"`
	SecretKeyPolly   string `mapstructure:"secret_key_polly"` // пароли  и ключи   убрать в env и не светить в конфиге
	RabbitUser       string `mapstructure:"rabbit_user"`
	RabbitPassword   string `mapstructure:"rabbit_password"`
	SoundFileDir     string `mapstructure:"sound_file_dir"`
	DbName           int    `mapstructure:"db_name"` //имя базы данны redis с hashe  и именем файла сохраненного распознанного
}

type IsOk int

const (
	OK IsOk = iota
	FAIL
)

const ( //Это нужно будет брать из конфига
	keyID            = "xxxxxx"
	serviceAccountID = "xxxxxxxxxxxxxxxx"
	keyFile          = "private.pem"
)

// func (s Suite) String() string {
// 	return [...]string{"OK", "FAIL"}[s]
// }

var conn *amqp.Connection
var log *logrus.Logger
var RedisClient *redis.Client

//var json = jsoniter.ConfigCompatibleWithStandardLibrary

func main() {

	//инициализация
	//var prod *viper.Viper                                           //current config  section
	var configFile = pflag.String("c", "config.json", "config file") //по умолчанию  config.json
	pflag.Parse()
	log = logrus.New()
	log.SetFormatter(&logrus.TextFormatter{
		DisableColors:   false,
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})

	var configurationInst Configuration

	//Проверяем и читаем конфиг

	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Fatal(err)
	}

	//вот здесь уже нужно  вернуть viper объект  для Unmarshal
	currentConfig, err := readConfig(filepath.Join(dir, *configFile))

	err = currentConfig.Unmarshal(&configurationInst)
	if err != nil {
		log.Fatalf("unable to decode into struct, %v", err)
	}
	log.Infof("Текущая конфигурация :  %+v", configurationInst)
	//на старте еще  нужно проверить  есть  ли необходимые  бинарники (lame и  sox)
	path, err := exec.LookPath("sox")
	if err != nil {
		log.Fatal("Can not find  SOX utility! Please  install: 'yum install sox' or  'apt install sox'")
	} else {
		log.Info("Found SOX: ", path)
	}
	path, err = exec.LookPath("lame")
	if err != nil {
		log.Fatal("Can not find  lame utility! Please  install: 'yum install lame' or  'apt install lame'")
	} else {
		log.Info("Found lame: ", path)
	}

	//  конец  инициализации
	//попробуем  сделать  exchange  point  с типом fanout
	////////////////////////////////////////////////////////////////
	//пробуем соедениться с redis
	RedisClient = redis.NewClient(&redis.Options{ // киент Redis concurently Safe и может быть использован из разных  go routine
		Addr:         "127.0.0.1:6379",         //нужно ли порт в конфиг ?
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
	for { //внешгий  цикл for
		//	fmt.Println("Connecting...")
		time.Sleep(1 * time.Second)
	OnError:
		for {
			log.Info("Connecting RabbitMQ...")
			connectionString := fmt.Sprintf("amqp://%[1]v:%[2]v@%[3]v:5672/", configurationInst.RabbitUser, configurationInst.RabbitPassword, configurationInst.HostRabbitServer)
			log.Info(connectionString)
			conn, err = amqp.Dial(connectionString)
			defer conn.Close()

			if err != nil {
				log.Println(err)
				time.Sleep(3 * time.Second)
				log.Info("Reconnecting  RabbitMQ....")
				continue
			} else {
				break OnError
			}

		}

		log.Info("Connected  RabbitMQ!!!")
		rabbitCloseError := make(chan *amqp.Error)
		notify := conn.NotifyClose(rabbitCloseError) //error channel

		ch, err := conn.Channel() //channel for consumer
		failOnError(err, "Failed to open a channel")
		defer ch.Close()
		q, err := ch.QueueDeclare(
			"rpc_queue", // name
			true,        // durable
			false,       // delete when usused
			false,       // exclusive
			false,       // no-wait
			nil,         // arguments
		)
		failOnError(err, "Failed to declare a queue")

		err = ch.Qos( //Qos  for consumer
			1,     // prefetch count
			0,     // prefetch size
			false, // global
		)
		failOnError(err, "Failed to set QoS")
		defer ch.Close()
		msgs, err := ch.Consume(
			q.Name, // queue
			"",     // consumer
			true,   // auto-ack  //потом поменять
			false,  // exclusive
			false,  // no-local
			false,  // no-wait
			nil,    // args
		)
		failOnError(err, "Failed to register a consumer")

		var d amqp.Delivery
		// конец

	GoReconnect:
		for { //внутренний цикл
			select {
			case err = <-notify:
				log.Info("LOST RabbitMQ  CONNECTION !!!")
				break GoReconnect //reconnect

			case d = <-msgs: //здесь  получим  результаты  их  надо записать в redis
				log.Info("recieve message!")
				var Dat = make(map[string]string) //мапа с данными

				//d.Ack(false)
				var RequestInst = new(RequestFile) //создаем конфиг по issue
				err := json.Unmarshal([]byte(d.Body), &RequestInst)
				if err != nil {
					log.Info("Error:", err)
					//log.Fatalf("%s: %s", d.Body, err)
					//d.Ack(false) //подтверждаем ТОЛЬКО текщее сообщение  вручную
					continue
					//os.Exit(1)  //поменял
				}
				message := RequestInst.Message
				fileName := RequestInst.Issue
				log.Info("Message: ", message, "FileName: ", fileName)
				//На будующее  если мы все равно  возвращаем имя  файла  из  сервиса  в принципе
				// можно  генерить его на стороне rpc сервера

				hashMessage := makeHash([]byte(message)) //делаем  хеш  из сообщения
				//может лучше проверять так ? if ok, _ := RedisClient.HExists(hashMessage, "FileName").Result(); ok {
				save_filename, err := RedisClient.HGet(hashMessage, "FileName").Result() //  сохраненное  значение ранее здесь должен быть json
				if err == nil && save_filename != "" {
					log.Info("Нашли  сохраненные  данные по этому issue!!!")
					Dat["Answer"] = "OK"
					Dat["FileName"] = save_filename
					body, err := json.Marshal(Dat)
					if err != nil {
						log.Info("Не удалось создать JSON")
						continue
					}

					err = ch.Publish(
						"",        // exchange
						d.ReplyTo, // routing key
						false,     // mandatory
						false,     // immediate
						amqp.Publishing{
							ContentType:   "application/json",
							CorrelationId: d.CorrelationId,
							Body:          []byte(body),
						})
					//failOnError(err, "Failed to publish a message")
					if err != nil {
						log.Info("Не удалось создать JSON")
						continue
					}

				} else { //создаем файл озвучки  если  нет в кеше
					err = makeWavFile(message, fileName, configurationInst.SoundFileDir, configurationInst.AccessKeyPolly, configurationInst.SecretKeyPolly)

					if err != nil {
						log.Info("файл Озвучки  не создан")

						Dat["Answer"] = "FAIL"
						body, err := json.Marshal(Dat)
						if err != nil {
							log.Info("Не удалось создать JSON")
							continue //в клиенте отвалимся  по таймауту
						}
						//и  отправляем назад сообщение что все хуево нужно  делать стандартную озвучку
						err = ch.Publish(
							"",        // exchange
							d.ReplyTo, // routing key
							false,     // mandatory
							false,     // immediate
							amqp.Publishing{
								ContentType:   "application/json",
								CorrelationId: d.CorrelationId,
								Body:          []byte(body),
							})
						//failOnError(err, "Failed to publish a message")
						if err != nil {
							log.Info("Failed to publish a message")
							continue //в клиенте отвалимся  по таймауту
						}

					} else {
						var Dat = make(map[string]string)
						hashMessage := makeHash([]byte(message))
						_, err = RedisClient.HSet(hashMessage, "FileName", fileName).Result()
						if err != nil {
							log.Info("Не удалось записать filename в redis")
						}

						_, err = RedisClient.HSet(hashMessage, "Message", message).Result()
						if err != nil {
							log.Info("Не удалось записать message в redis")
						}
						Dat["Answer"] = "OK"
						Dat["FileName"] = fileName
						body, err := json.Marshal(Dat)
						if err != nil {
							log.Info("Не удалось создать JSON")
							continue
						}
						log.Info("структура  перед отправкой: ", Dat)

						err = ch.Publish(
							"",        // exchange
							d.ReplyTo, // routing key
							false,     // mandatory
							false,     // immediate
							amqp.Publishing{
								ContentType:   "application/json",
								CorrelationId: d.CorrelationId,
								Body:          []byte(body),
							})

						if err != nil {
							log.Info("Не удалось создать JSON")
							continue
						}

						//d.Ack(false)

					}
					//отправляем назад  сообщение "Ok" типа файл создан  можно  конечно на всяк случай проверить что  файл  создан
					//if _, err := os.Stat("/path/to/whatever"); os.IsNotExist(err) {
					//	// path/to/whatever does not exist
					//  }
				} //else  если файл озвучки не найден

			} // это весь select конец SELECT
		} // внутренний

	} //внешний цикл

	///////////////////////////////////////////////////////////////
}


func makeWavFile(message string, idFile string, prefix string, accessKey string, secretKey string) error { //возвращаем ошибку если не смогли создать файл

	var cyrillicPresent bool

	//	var b2 bytes.Buffer
	//смотрим есть ли  кириллица в сообщении, если нет , то используем English native  language speaker для генерации файла озвучки
	for _, r := range []rune(message) {
		if unicode.Is(unicode.Cyrillic, r) {
			cyrillicPresent = true
			break
		}
	}

	var polly = golang_tts.New(accessKey, secretKey)

	var file_out_wav = filepath.Join(prefix, idFile+".wav")

	waveFile, err := os.OpenFile(file_out_wav, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755) //здесь будет wav file
	if err != nil {
		log.Error(err)
		return err
	}
	defer waveFile.Close() //не забываем  закрывать  файл

	polly.Format(golang_tts.PCM)
	if cyrillicPresent {
		polly.Voice(golang_tts.Tatyana)
		log.Info("Используем великий могучий при распознавании")

	} else {
		polly.Voice(golang_tts.Emma)
		log.Info("Используем English only")
	}

	text := message

	bytes_pcm, err := polly.Speech(text)

	if err != nil {
		log.Error(err)
		return errors.New("Error  make  voice  file")
	}

	//
	waveEncoder := wav.NewEncoder(waveFile, 8000, 16, 1, 1)

	// Create new audio.IntBuffer.
	audioBuf, err := newAudioIntBuffer(bytes.NewReader(bytes_pcm))
	if err != nil {
		log.Error(err)
		return errors.New(err.Error())
	}
	// Write buffer to output file. This writes a RIFF header and the PCM chunks from the audio.IntBuffer.
	if err := waveEncoder.Write(audioBuf); err != nil {
		log.Error(err)
	}
	if err := waveEncoder.Close(); err != nil {
		log.Error(err)
		return errors.New(err.Error())
	}

	//
	// if err = LpcmToAlaw(file_out, file_out_alaw); err != nil {
	// 	return err

	// }
	return nil
}

//
func newAudioIntBuffer(r io.Reader) (*audio.IntBuffer, error) {
	buf := audio.IntBuffer{
		Format: &audio.Format{
			NumChannels: 1,
			SampleRate:  8000,
		},
	}
	for {
		var sample int16
		err := binary.Read(r, binary.LittleEndian, &sample)
		switch {
		case err == io.EOF:
			return &buf, nil
		case err != nil:
			return nil, err
		}
		buf.Data = append(buf.Data, int(sample))
	}
}

//дописать таки вариант распознавания с YANDEX!!!
func makeVoiceFileYandex(message string, idFile string, prefix string, accessKey string, secretKey string) error {

	//	var b2 bytes.Buffer

	var polly = golang_tts.New(accessKey, secretKey)

	var file_out_wav = prefix + idFile + ".wav"
	waveFile, err := os.OpenFile(file_out_wav, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755) //здесь будет wav file
	if err != nil {
		fmt.Println(err)
		return err
	}
	defer waveFile.Close()

	polly.Format(golang_tts.PCM)
	polly.Voice(golang_tts.Tatyana)

	text := message

	bytes_pcm, err := polly.Speech(text)

	if err != nil {
		fmt.Println(err)

		return errors.New("Error  make  voice  file")
	}

	//
	waveEncoder := wav.NewEncoder(waveFile, 8000, 16, 1, 1)

	// Create new audio.IntBuffer.
	audioBuf, err := newAudioIntBuffer(bytes.NewReader(bytes_pcm))
	if err != nil {
		log.Fatal(err)
	}
	// Write buffer to output file. This writes a RIFF header and the PCM chunks from the audio.IntBuffer.
	if err := waveEncoder.Write(audioBuf); err != nil {
		log.Fatal(err)
	}
	if err := waveEncoder.Close(); err != nil {
		log.Fatal(err)
	}

	//

	// if err = LpcmToAlaw(file_out, file_out_alaw); err != nil {
	// 	return err

	// }
	return nil
}

//function for  convert  struct  to map
func structToMap(i interface{}) (values url.Values) {
	values = url.Values{}
	iVal := reflect.ValueOf(i).Elem()
	typ := iVal.Type()
	for i := 0; i < iVal.NumField(); i++ {
		f := iVal.Field(i)
		// You ca use tags here...
		// tag := typ.Field(i).Tag.Get("tagname")
		// Convert each type into a string for the url.Values string map
		var v string
		switch f.Interface().(type) {
		case int, int8, int16, int32, int64:
			v = strconv.FormatInt(f.Int(), 10)
		case uint, uint8, uint16, uint32, uint64:
			v = strconv.FormatUint(f.Uint(), 10)
		case float32:
			v = strconv.FormatFloat(f.Float(), 'f', 4, 32)
		case float64:
			v = strconv.FormatFloat(f.Float(), 'f', 4, 64)
		case []byte:
			v = string(f.Bytes())
		case string:
			v = f.String()
		}
		//values.Set(strings.ToLower(typ.Field(i).Name), v)
		values.Set(LcFirst(typ.Field(i).Name), v)
	}
	return
}

// Function  for  ToLower first  char in string
func LcFirst(str string) string {
	for i, v := range str {
		return string(unicode.ToLower(v)) + str[i+1:]
	}
	return ""
}

//using: bv := []byte("Моя  большая строка для распознавания текста Text  сервис  астериск ")
//fmt.Println(storee(bv))
// func for  make hash from []byte . Return string Use for make HASH from sting and cache  voice file
func makeHash(bv []byte) string {
	hasher := sha1.New()
	hasher.Write(bv)
	sha := base64.URLEncoding.EncodeToString(hasher.Sum(nil))
	return sha
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
		//os.Exit(1)
	}
	log.Info("Current env: ", currentEnv)

	if !viper.IsSet(currentEnv + ".host_rabbit_server") {
		log.Error("missing  host_rabbit_server")
		return currentConfig, errors.New("missing host_rabbit_server")
	}

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
	if !viper.IsSet(currentEnv + ".access_key_polly") {
		log.Error("missing access_key_polly")
		return currentConfig, errors.New("missing access_key_polly")
	}
	if !viper.IsSet(currentEnv + ".secret_key_polly") {
		log.Error("missing secret_key_polly")
		return currentConfig, errors.New("missing secret_key_polly")
	}
	if !viper.IsSet(currentEnv + ".db_name") {
		log.Error("missing db_name")
		return currentConfig, errors.New("missing db_name")
	}
	//

	return currentConfig, nil

}

//
func isFlagPassed(name string) bool {
	found := false
	pflag.Visit(func(f *pflag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

//если  нужны  будут  обязательные  ключи
func isAllMandatoryFlagsPresents(mandatoryFlags []string) (bool, string) {
	for _, element := range mandatoryFlags {
		if !isFlagPassed(element) {
			fmt.Println("Element not found: ", element)
			return false, element

		}
	}
	return true, ""
}
