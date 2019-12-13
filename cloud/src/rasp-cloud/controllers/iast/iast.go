//Copyright 2017-2019 Baidu Inc.
//
//Licensed under the Apache License, Version 2.0 (the "License");
//you may not use this file except in compliance with the License.
//You may obtain a copy of the License at
//
//http: //www.apache.org/licenses/LICENSE-2.0
//
//Unless required by applicable law or agreed to in writing, software
//distributed under the License is distributed on an "AS IS" BASIS,
//WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//See the License for the specific language governing permissions and
//limitations under the License.

package iast

import (
	"encoding/json"
	"errors"
	"github.com/astaxie/beego"
	"github.com/gorilla/websocket"
	"net/http"
	"rasp-cloud/controllers"
	"rasp-cloud/environment"
	"rasp-cloud/models"
	"strings"
	"sync"
	"time"
)

type IastController struct {
	controllers.BaseController
}

type WebsocketController struct {
	controllers.BaseController
}

type wsConnection struct {
	wsSocket      *websocket.Conn // 底层websocket
	inChan        chan *models.WsMessage // 写IAST
	outChan       chan *models.WsMessage // 读IAST
	mutex         sync.Mutex // Mutex互斥锁，避免重复关闭管道
	isClosed      bool
	closeChan     chan byte // 关闭通知
	appId         string
	registerMutex sync.Mutex
}

var (
	wsUpgrader = websocket.Upgrader{
		ReadBufferSize:    4096,
		WriteBufferSize:   4096,
		HandshakeTimeout:  5 * time.Second,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	IastConnection = make(map[string]*wsConnection)
)

// @router / [post]
func (o *IastController) Post() {
	var param struct {
		Order string         `json:"order"`
		Data  *models.Iast   `json:"data" `
	}
	var result = make(map[string]interface{})
	var wsConn *wsConnection
	quit := make(chan bool)
	o.UnmarshalJson(&param)

	if param.Data == nil {
		o.ServeError(http.StatusBadRequest, "search data can not be empty")
	}
	if param.Data.AppId == "" {
		o.ServeError(http.StatusBadRequest, "app_id can not be empty")
	}
	if param.Order == "" {
		o.ServeError(http.StatusBadRequest, "order can not be empty")
	}
	if strings.Index(o.Ctx.Input.Header("Content-Type"), "application/json") == -1 {
		o.ServeError(http.StatusUnsupportedMediaType, "Unsupported Media Type")
	}

	appId := param.Data.AppId
	result["register"] = models.IastRegister[appId]
	if models.IastAppId[appId] {
		wsConn = IastConnection[appId]
		if err := wsConn.wsWrite(websocket.TextMessage, o.Ctx.Input.RequestBody); err != nil {
			beego.Error("send msg from web failed!")
			defer wsConn.wsClose()
			o.ServeError(http.StatusBadRequest, "IAST is not started!")
		}
		go func() {
			for {
				select {
				// 取一个应答
				case msg := <-models.ResChan:
					if string(msg) != "" {
						beego.Info("msg:", string(msg))
						if err := json.Unmarshal(msg, &result); err != nil {
							o.ServeError(http.StatusBadRequest, "Invalid JSON from iast", err)
						}
					}
					quit <- true
					goto quit
				case <- time.After(3 * time.Second):
					o.ServeError(http.StatusBadRequest, "TimeOut Recv Data From IAST!")
					quit <- true
					goto lable
				}
			}
		lable:
			beego.Info("enter lable")
		quit:
		}()

		<- quit
		o.Serve(result)
	}
	// 扫描器未连接
	o.Serve(result)
}

// @router / [get]
func (o *WebsocketController) Get() {
	wsSocket, err := wsUpgrader.Upgrade(o.Ctx.ResponseWriter, o.Ctx.Request, nil)
	if err != nil {
		beego.Error("upgrade err:", err)
	}
	appId := o.Ctx.Request.Header["X-Openrasp-Appid"][0]
	wsConn := &wsConnection{
		wsSocket:  wsSocket,
		inChan:    make(chan *models.WsMessage, 1000),
		outChan:   make(chan *models.WsMessage, 1000),
		closeChan: make(chan byte),
		isClosed:  false,
		appId:     appId,
	}

	if !models.IastAppId[appId] {
		models.InChan = wsConn.inChan
		models.OutChan = wsConn.outChan
		models.CloseChan = wsConn.closeChan
		models.IsClosed = wsConn.isClosed
		models.IastAppId[appId] = true
		IastConnection[appId] = wsConn
		// 处理器
		go wsConn.procLoop(appId)
		// 读协程
		go wsConn.wsReadLoop(appId)
		// 写协程
		go wsConn.wsWriteLoop()
	} else {
		msgType, _, err := wsConn.wsSocket.ReadMessage()
		if err != nil {
			beego.Error("ReadMessage error:", err)
		}
		msg := &models.WsMessage{
			msgType,
			[]byte("=== app_id already exist! ==="),
		}
		if err := wsConn.wsSocket.WriteMessage(msg.MessageType, msg.Data); err != nil {
			beego.Error("WriteMessage error:", err)
		}
		wsConn.wsCloseDup()
	}

}

// @router /version [post]
func (o *IastController) Version() {
	result := make(map[string]interface{})
	result["version"] = environment.Version
	o.Serve(result)
}

// @router /auth [post]
func (o *IastController) Auth() {
	o.ServeWithEmptyData()
}

func (wsConn *wsConnection) wsWrite(messageType int, data []byte) error {
	select {
	case wsConn.outChan <- &models.WsMessage{messageType, data}:
	case <-wsConn.closeChan:
		return errors.New("websocket closed")
	}
	return nil
}

func (wsConn *wsConnection) wsRead() (*models.WsMessage, error) {
	select {
	case msg := <-wsConn.inChan:
		return msg, nil
	case <-wsConn.closeChan:
	}
	return nil, errors.New("websocket closed")
}

func (wsConn *wsConnection) wsReadLoop(appId string) {
	for {
		msgType, data, err := wsConn.wsSocket.ReadMessage()
		if err != nil {
			beego.Error("ReadMessage error:", err)
			goto error
		}
		dataStr := string(data)
		if len(dataStr) != 0 {
			req := &models.WsMessage{}
			if dataStr == "startup" {
				wsConn.registerMutex.Lock()
				defer wsConn.registerMutex.Lock()
				models.IastRegister[appId] = 1
				req = &models.WsMessage{
					msgType,
					[]byte("=== send heartbeat and building ws connections ==="),
				}
			} else {
				req = &models.WsMessage{
					msgType,
					[]byte(dataStr),
				}
			}
			// 放入请求队列
			select {
			case wsConn.inChan <- req:
			case <-wsConn.closeChan:
				goto closed
			}
		}
	}
error:
	wsConn.wsClose()
closed:
	beego.Info("websocket is closed.")
}

func (wsConn *wsConnection) wsWriteLoop() {
	for {
		select {
		// 取一个应答
		case msg := <-wsConn.outChan:
			// 写给websocket
			if err := wsConn.wsSocket.WriteMessage(msg.MessageType, msg.Data); err != nil {
				beego.Error("WriteMessage error:", err)
				goto error
			}
		case <-wsConn.closeChan:
			goto closed
		}
	}
error:
	wsConn.wsClose()
closed:
	beego.Info("websocket is closed.")
}

func (wsConn *wsConnection) procLoop(appId string) {
	go func() {
		for {
			models.IastRegister[appId] = 2
			time.Sleep(3 * time.Second)
			if err := wsConn.wsWrite(websocket.TextMessage, []byte("heartbeat from OpenRASP cloud")); err != nil {
				beego.Error("heartbeat error:", err)
				wsConn.wsClose()
				break
			}
		}
	}()

	for {
		msg, err := wsConn.wsRead()
		if err != nil {
			beego.Error("read error:" , err)
			break
		}
		if strings.Index(string(msg.Data), "status") == -1 {
			err = wsConn.wsWrite(msg.MessageType, msg.Data)
			if err != nil {
				beego.Error("write error:", err)
				break
			}
		} else {
			models.ResChan <- msg.Data
		}
	}
}

func (wsConn *wsConnection) wsClose() {
	appId := wsConn.appId
	wsConn.wsCloseDup()
	models.IastAppId[appId] = false
	models.IastRegister[appId] = 0
}

func (wsConn *wsConnection) wsCloseDup() {
	wsConn.mutex.Lock()
	defer wsConn.mutex.Unlock()
	if !wsConn.isClosed {
		wsConn.isClosed = true
		close(wsConn.closeChan)
	}
	err := wsConn.wsSocket.Close()
	if err != nil {
		beego.Error("close ws err:", err)
	}
}