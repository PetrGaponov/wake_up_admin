package main

/* TODO:
нужно  сделать  pir   через  который мы звоним  параметром который  возможно  будет  подставляться автоматом в
зависимости от route в запросе REST
*/

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/CyCoreSystems/ari/ext/play"
	"github.com/CyCoreSystems/ari/ext/record"
	"github.com/CyCoreSystems/ari/rid"

	//"github.com/prometheus/common/log"
	"golang.org/x/net/context"

	"github.com/CyCoreSystems/ari"

	"github.com/sirupsen/logrus"
)

var exitChan = make(chan int) //канал для выхода  нужно  2 канала или 1 хватит ?

//var phone string
//AriClient  нужно  сделать  интерфейсом
//Ari Client   сюда  надо  захуячить мьютексы для доступа к разным мапам
type AriClientBridge struct {
	cl             ari.Client
	clientChannel  map[string]string            //соответствие  между клиентским каналом и каналом  ivr
	ivrChannel     map[string]string            //MAP  канал IVR - Канал клиента
	bridgeHandle   map[string]*ari.BridgeHandle //здесь сохраняем id bridge что бы потом добавить туда канал от IVR.  ключ -  id канала ДО IVR
	chanId         map[string](chan int)        //  это  каналы  ключи в которых это id channels Клиента  а value  сам channel для связи с handler ом клиента
	chanIsclosed   bool                         //это для проверки того что канал связи  закрыт
	muChanClosed   sync.Mutex
	log            *logrus.Logger
	endpoint       string
	endpointIvr    string
	firstNumber    string //это 2 телефона которые нужно  объеденить в  bridge первый это как бы райдер
	secondNumber   string
	issue          string
	NoAnswerPhone1 bool //если  дозвонились до первого абонента
	NoAnswerPhone2 bool // если дозвонились до второго абонента
	SuccessDialog  bool //если  удалось  установить bridge
	doOnce         sync.Once
	LastCallTime   time.Time `json:"last_call_time"` //здесь будем  хранить последнее время звонка

}

func (ariClient *AriClientBridge) SafeClose(id string) { //функция  для  безопасного закрытия  канала связи
	ariClient.muChanClosed.Lock()
	defer ariClient.muChanClosed.Unlock()
	if !ariClient.chanIsclosed {
		ariClient.log.Info("Закрыли  канал связи")
		close(ariClient.chanId[id])
		ariClient.chanIsclosed = true
	}
	ariClient.log.Info("Канал был уже закрыт")
}

func (ariClient *AriClientBridge) IsClosed() bool { //helper  для  проверки
	ariClient.muChanClosed.Lock()
	defer ariClient.muChanClosed.Unlock()
	return ariClient.chanIsclosed
}

//
func (a *AriClientBridge) listenAppBridge(ctx context.Context, cl ari.Client, handler func(ctx context.Context, h *ari.ChannelHandle)) { //добавил ctx
	sub := cl.Bus().Subscribe(nil, "StasisStart") //листнер  оставляем  запущенным
	end := cl.Bus().Subscribe(nil, "StasisEnd")   //подписываемся на события старта и стопа звонка

	for {
		select {
		case e := <-sub.Events(): //Если  пришло  событие Stasis Start  Вот здесь  нужно создавать контекст!!!!

			v := e.(*ari.StasisStart)
			a.log.Info("Got stasis start ", "channel ", v.Channel.ID)
			//ctxCl, cancel := context.WithCancel(context.Background())                         //создаем контекст для клиентского вызова! Подумать как его передать во второй case!
			go handler(ctx, a.cl.Channel().Get(v.Key(ari.ChannelKey, v.Channel.ID))) //Запускаем handler
		case t := <-end.Events():
			a.log.Info("Got stasis end")
			a.log.Info("пришло событие StasisStop в каком то канале ")
			v2 := t.(*ari.StasisEnd)
			_, ok := a.clientChannel[v2.Channel.ID] // что это за канал client или ivr ?
			if ok {
				a.log.Info(" Stasis end от клиента ")
				a.log.Info(v2.Channel.ID) //for debug выводим id канала
				//chanId[v2.Channel.ID] <- 1
				//_, ok1 := <-chanId[v2.Channel.ID]
				_, ok2 := a.chanId[v2.Channel.ID] // после Hangup  может придти ПОВТОРНО событие StasisEnd , проверяем что канала уже нет что бы не словить stacktrace
				if !ok2 {
					a.log.Info("Канал уже закрыт")
				} else {
					a.SafeClose(v2.Channel.ID)

					//close(a.chanId[v2.Channel.ID])
				}

			} else { //надо везде  ставить mutex иначе  будет пиздец
				//_, ok1 := <-chanId[ivrChannel[v2.Channel.ID]]
				_, ok2 := a.chanId[a.ivrChannel[v2.Channel.ID]] //после Hangup  может придти ПОВТОРНО событие StasisEnd , проверяем что канала уже нет что бы не словить stacktrace
				a.log.Info("stasis end от IVR")
				a.log.Info(a.ivrChannel[v2.Channel.ID])
				//chanId[ivrChannel[v2.Channel.ID]] <- 1
				if !ok2 {
					a.log.Info("Канал уже закрыт")

				} else {

					//close(a.chanId[a.ivrChannel[v2.Channel.ID]])
					a.SafeClose(a.ivrChannel[v2.Channel.ID])
				}

			}

		case <-ctx.Done():
			exitChan <- 1
			return
		}
	}

}

func (a *AriClientBridge) createCallBridge(number string, endpoint string) (h *ari.ChannelHandle, err error) { //Клиент cl  принимает участие и в дозвоне  и в обработке . В каждой функции  у нас клиент контекст и ChannelHandler
	/*h, err = cl.Channel().Create(nil, ari.ChannelCreateRequest{  //это для "Asterisk  14 и 16" если таки решим что нужны  расширенные Ringing State . Тогда еще над handler нужно Dial вызвать !
		Endpoint: endpoint + number,                               //к сожалению, все это не имеет смысла , если звоним через промежуточный PIR. Состояния тогда не отдаются  !
		App:      phone,
	})*/
	h, err = a.cl.Channel().Originate(nil, ari.OriginateRequest{
		Endpoint: endpoint + number,
		App:      a.firstNumber + "_bridge", //  здесь   нужно подставлять номер клиент
		Timeout:  20,                        // говорим сколько звонить
		//CallerID: "7499xxxxx", //возможно нам будут передавать caller id с которого звонить надо
	})
	a.doOnce.Do(func() { //устанавливает время  лишь однажды при первом звонке !!
		loc, _ := time.LoadLocation("Europe/Moscow")
		now := time.Now()
		a.LastCallTime = time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), loc)

	})

	return
}

func (a *AriClientBridge) createCallToIvr(number string, endpoint string, clientNumber string) (h *ari.ChannelHandle, err error) { //Клиент cl  принимает участие и в дозвоне  и в обработке . В каждой функции  у нас клиент контекст и ChannelHandler
	/*h, err = cl.Channel().Create(nil, ari.ChannelCreateRequest{  //это для "Asterisk  14" если таки решим что нужны  расширенные Ringing State . Тогда еще над handler нужно Dial вызвать !
		Endpoint: endpoint + number,                               //к сожалению, все это не имеет смысла , если звоним через промежуточный PIR. Состояния тогда не отдаются  !
		App:      phone,
	})*/
	h, err = a.cl.Channel().Originate(nil, ari.OriginateRequest{
		Endpoint: endpoint + number,
		App:      a.firstNumber + "_bridge", // здесь  нужно подставлять номер IVR
		Timeout:  20,                        // говорим сколько звонить
		CallerID: clientNumber,              //возможно нам будут передавать caller id с которого звонить надо
	})

	return
}

//

func (a *AriClientBridge) channelHandlerBridge(ctx context.Context, h *ari.ChannelHandle) { //добавил context
	//Этот  handler  вызывается  видимо  при  поднятии канала
	a.log.Info("Running channel handler")

	data, err := h.Data()
	if err != nil {
		a.log.Error("Error getting data", "error", err)
		return
	}
	a.log.Info("Channel State", "state", data.State)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {

		a.log.Info("Handler RUN. Channel is up!?")
		//timer := time.NewTimer(time.Second * 7)
		data, err = h.Data()
		if err != nil {
			a.log.Error("Error getting data", "error", err)
			//continue
			return
		}
		a.log.Info("State is : ", data.State)
		//
		if data.State == "Up" { //Если используем originate и aster до 13 включительно, Handler стартанет только после поднятия канала

			//Здесь
			a.log.Info("Зашли в UP!!")
			value, ok := a.clientChannel[h.ID()] // что это за канал client или ivr ?
			if ok {
				a.log.Info("Key found value is: ", value, " Это канал клиента ")
			} else {
				a.log.Info("Key not found. Вероятно это канал до IVR")
				//далее здесь обрабатываем звонок  на второй номер
				_, ok := a.bridgeHandle[h.ID()] // есть по этому id ссылка на bridge ?
				if ok {
					a.log.Info("Хуясе!! нашли наш bridge в вызове ivr ! ")
					//далее надо добавить этот  id канала от ivr в bridge который возьмем из map
					//останавливаем MOH в bridge если таки дозвонились
					if err := a.bridgeHandle[h.ID()].StopMOH(); err != nil { //останавливаем MOH у первого абонента который ждет...
						a.log.Error("failed to stop bridge", "error", err)
					}
					//
					if err = a.bridgeHandle[h.ID()].AddChannel(h.Key().ID); err != nil {
						a.log.Error("failed to add channel to bridge в IVR", "error", err)
						return
					} else {

						a.log.Info("Успешно  добавили канал IVR в bridge ")
						brData2, err := a.bridgeHandle[h.ID()].Data()
						if err != nil {
							a.log.Error("Failed to get DATA", "error", err)
							return
						}
						listOfChannels := brData2.Channels()
						a.log.Info("Список каналов : ", listOfChannels)
						a.log.Info("Колличество каналов в бридже  : ", len(listOfChannels))

					}
					// после разблокировки нужно  удалить bridge
					a.log.Info("Заблокировались в IVR по ID ", a.ivrChannel[h.ID()])
					_, ok := a.chanId[a.ivrChannel[h.ID()]]
					if ok {
						a.log.Info("Ключ в канале для IVR найден ") //for debug
					} else {
						a.log.Info("Ключ в канале для IVR НЕ найден Ошибка!!! ")
					}
					//
					if err := play.Play(ctx, h, play.URI("sound:connect_complite")).Err(); err != nil {
						log.Error("failed to play sound in IVR", "error", err)
						return
					}

					a.log.Info("completed playback all")
					//если  добежали  сюда  помечаем что дозвонились до второго   абонента и диалог успешный
					a.NoAnswerPhone2 = false
					a.SuccessDialog = true

					//
					<-a.chanId[a.ivrChannel[h.ID()]] //берем номер канала клиента из соответствия Канал для блокировки у нас по ID канала клиента
					//<-chan_ivr[h.ID()]
					delete(a.ivrChannel, h.ID()) //удаляем из map  соответствие ivr и клиентского канала
					a.log.Info("После блокировки в канале дозвона IVR")
					wg.Done() //после блокировки
					return    //именно ТАК!!  дальше не должны пойти т к далее идет работа с каналом первого абонента

				} else {
					a.log.Info("Ключ от ivr не нашли .")
					wg.Done()

					return
				}

			}
			//таки bridge  нужно  создавать  сразу при звонке клиенту а не здесь в основной Go routine
			//TODO :  перенести это потом !!!
			//Здесь нужно  пробовать создать bridge
			src := h.Key()
			key := src.New(ari.BridgeKey, rid.New(rid.Bridge))

			bridge, err := a.cl.Bridge().Create(key, "mixing", key.ID) //Создаем bridge

			if err != nil {
				bridge = nil
				return //errors.Wrap(err, "failed to create bridge")

			}

			//Далее добавляем канал в bridge!!
			if err := bridge.AddChannel(h.Key().ID); err != nil {
				a.log.Error("failed to add channel to bridge", "error", err)
				a.log.Info("failed to add channel to bridge")
				return
			}
			//здесь  можно  сказать  что идет соединение с клиентом
			//Добавил код
			//	ctxPlay, _ := context.WithCancel(context.Background())
			if err := play.Play(ctx, bridge, play.URI("sound:client")).Err(); err != nil { //идет соединение с клиентом
				log.Error("failed to play sound", "error", err)
				return
			}

			if err = bridge.MOH("default"); err != nil { //идет соединение с клиентом  останавливаем  ПОТОМ  h.StopMOH()
				log.Error("failed to play MOH sound ", "error: ", err)

			}
			a.NoAnswerPhone1 = false //   до первого дозвонились но  пока не дозвонимся  до второго  непонятно был ли диалог успешный

			a.log.Info("completed playback all")

			//
			// Здесь расчитываем что IVR возьмет трубку ПОЛЮБОМУ, иначе надо предусмотреть таймаут

			hIvr, err := a.createCallBridge(a.secondNumber, a.endpointIvr) //звоним
			//здесь надо проверить ошибку
			//chan_ivr[hIvr.ID()] = make(chan int)                             //
			a.ivrChannel[hIvr.ID()] = h.ID() //добавляем соответствие между каналом ivr и клиентским
			a.log.Info("добавили соответствие id IVR CLIENT ", hIvr.ID(), " ", h.ID())
			a.log.Info(runtime.NumGoroutine())

			if err != nil {
				a.log.Error("Failed to create call", "error", err)
				return
			}
			a.clientChannel[h.ID()] = hIvr.ID() //добавляем соответствие каналов
			a.log.Info("Channel id  from originate is:  ", h.ID())
			a.bridgeHandle[hIvr.ID()] = bridge //сохраняем  наш bridge с key  id  канала до ivr

			//
			a.log.Info("Все после создания bridge!!!")
			brData, err := a.bridgeHandle[hIvr.ID()].Data()
			if err != nil {
				a.log.Error("Failed to get DATA", "error", err)
				return
			}
			//вот так  можно проверять что  дозвонились до ivr
			//по длине  list каналов в bridge
			//взять тайм аут , и после проверить что длина 2, если нет, то уходим
			//можно что то сказать клиенту, типа сорян, что то пошло не так
			listOfChannels := brData.Channels()
			a.log.Info("Список каналов : ", listOfChannels)

			// это  пока не нужно
			go func() { // здесь нужно ПРИМЕРНО через  25 сек проверить есть ли дозвон до второго  абонента ?
				time.Sleep(25 * time.Second)
				brData, err := bridge.Data() //берем данные из  НАШЕГО bridge
				if err != nil {
					a.log.Info("не дозвонились до второго абонента, выходим")
					a.SafeClose(h.ID())
					return
				}
				listOfChannels := brData.Channels()
				if len(listOfChannels) < 2 {
					//не дозвонилист до второго абонента , сворачиваемся
					a.log.Info("не дозвонились до второго абонента, выходим")
					a.NoAnswerPhone2 = true
					a.SuccessDialog = false
					a.SafeClose(h.ID())

				}

			}()
			var sessionRec record.Session //сессия
			var res *record.Result        // результат здесь  заблокируемся
			var sessionError error

			//  Далее  можно  запустить  запись   а после  разблокировки   сохранить ее Можно   предусмотреть опциональную  запись/не запись
			go func() { // рутина   для запуска  записи
				// res, err := record.Record(ctx, bridge,
				// 	record.TerminateOn("none"),
				// 	record.IfExists("overwrite"), //"append"  попробовать
				// ).Result()
				sessionRec = record.Record(ctx, bridge, record.TerminateOn("none"), record.IfExists("overwrite")) //Получаем сессию
				a.log.Info("Res error of session: ", sessionRec.Err())
				res, sessionError = sessionRec.Result() //блокируемся
				if sessionError != nil {
					log.Error("failed to record", "error", err)
					return
				}

			}()

			//

			<-a.chanId[h.ID()]
			a.log.Info("После блокировки")
			//прибираемся за собой
			if err = bridge.StopMOH(); err != nil { //идет соединение с клиентом  останавливаем  h.StopMOH()
				log.Error("failed to stop MOH sound after block ", "error: ", err) //пробуем  остановить MOH здесь если небыло дозвона до второго абонента

			}
			//вот здесь надо проверить что значение не nil
			if sessionError == nil {
				//готовим имя файла
				//
				log.Info("Нет ошибки в записи  поэтому делаем запись  файла....")
				s := time.Now()
				fileName := fmt.Sprintf("%v_%v_%v", a.firstNumber, a.secondNumber, s.Format("2006-01-02_15:04:05"))
				resStop := sessionRec.Stop()
				a.log.Info("Результат  Stop: ", resStop.Error)
				a.log.Info(resStop.Error == nil)
				a.log.Info("After  stop ")
				if err = res.Save(fileName); err != nil {
					log.Error("failed to save recording", "error", err)
				}

			}

			a.bridgeHandle[hIvr.ID()].Delete() //удаляем  bridge
			delete(a.bridgeHandle, hIvr.ID())  //чистим за собой удаляем из map bridge
			delete(a.clientChannel, h.ID())    // чистим за собой удаляем из map client id chan

			wg.Done()
			a.log.Info("После exitChan")
			time.Sleep(time.Second)

			h.Hangup() //делаем  hangup   канала на всякий случай

			return
		}

		//

		defer wg.Done()

		//код
	}()

	//h.Answer()

	wg.Wait()
	a.log.Info("Выход  из handler")

	//h.Hangup()
	return
}

type AnswerToRabbitBridge struct {
	Issue        string    `json:"issue"`
	Phone1       string    `json:"phone1"`
	Phone2       string    `json:"phone2"`
	LastCallTime time.Time `json:"last_call_time"` //здесь будем  хранить последнее время звонка
	Phone1Status string    `json:"phone1_status"`  //здесь храним статус ответа по каждому номеру
	Phone2Status string    `json:"phone2_status"`
	DialogStatus string    `json:"dialog_status"` //здесь какой то результат по диалогу
}

//конец определения методов
