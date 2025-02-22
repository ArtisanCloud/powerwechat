package kernel

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	logger2 "github.com/ArtisanCloud/PowerLibs/v3/logger"
	"github.com/ArtisanCloud/PowerLibs/v3/object"
	"github.com/ArtisanCloud/PowerWeChat/v3/src/kernel/contract"
	"github.com/ArtisanCloud/PowerWeChat/v3/src/kernel/messages"
	"github.com/ArtisanCloud/PowerWeChat/v3/src/kernel/models"
	"github.com/ArtisanCloud/PowerWeChat/v3/src/kernel/support"
)

const SUCCESS_EMPTY_RESPONSE = "success"

var MESSAGE_TYPE_MAPPING = map[string]int{
	"*":               messages.VOID,
	"text":            messages.TEXT,
	"image":           messages.IMAGE,
	"voice":           messages.VOICE,
	"video":           messages.VIDEO,
	"shortvideo":      messages.SHORT_VIDEO,
	"location":        messages.LOCATION,
	"link":            messages.LINK,
	"device_event":    messages.DEVICE_EVENT,
	"device_text":     messages.DEVICE_TEXT,
	"event":           messages.EVENT,
	"file":            messages.FILE,
	"miniprogrampage": messages.MINIPROGRAM_PAGE,
}

type ServerGuard struct {
	*support.Observable
	*support.ResponseCastable

	alwaysValidate bool
	App            *ApplicationInterface

	IsSafeMode              func(request *http.Request) bool
	Validate                func(request *http.Request) (*ServerGuard, error)
	ShouldReturnRawResponse func(request *http.Request) bool
	RequestDataType         func(request *http.Request) string

	ToCallbackType func(callbackHeader contract.EventInterface, buf []byte) (decryptMessage interface{}, err error)

	GetToken    func() string
	Resolve     func(request *http.Request) (httpRS *http.Response, err error)
	Notify      func(request *http.Request, closure func(event contract.EventInterface) interface{}) (response *http.Response, err error)
	HandleEvent func(request *http.Request, closure func(event contract.EventInterface) interface{}) (*object.HashMap, error)
}

func NewServerGuard(app *ApplicationInterface) *ServerGuard {
	serverGuard := &ServerGuard{
		Observable: support.NewObservable(),
		App:        app,
	}

	serverGuard.IsSafeMode = func(request *http.Request) bool {
		return serverGuard.isSafeMode(request)
	}
	serverGuard.Validate = func(request *http.Request) (*ServerGuard, error) {
		return serverGuard.validate(request)
	}
	serverGuard.ShouldReturnRawResponse = func(request *http.Request) bool {
		return serverGuard.shouldReturnRawResponse(request)
	}
	serverGuard.RequestDataType = func(request *http.Request) string {
		return serverGuard.requestDataType(request)
	}

	serverGuard.OverrideGetToken()
	serverGuard.OverrideResolve()
	serverGuard.OverrideNotify()
	serverGuard.OverrideHandleEvent()

	return serverGuard
}

func (serverGuard *ServerGuard) OverrideNotify() {
	serverGuard.Notify = func(request *http.Request, closure func(event contract.EventInterface) interface{}) (response *http.Response, err error) {
		validatedGuard, err := serverGuard.Validate(request)
		if err != nil {
			return nil, err
		}

		response, err = validatedGuard.ResolveEvent(request, closure)

		return response, err
	}
}

// 回调配置
// https://developer.work.weixin.qq.com/document/path/90930
func (serverGuard *ServerGuard) VerifyURL(request *http.Request) (httpRS *http.Response, err error) {
	logger := (*serverGuard.App).GetComponent("Logger").(*logger2.Logger)

	//_, err = serverGuard.Validate(request)
	//if err != nil {
	// return nil, err
	//}
	strEcho := request.URL.Query().Get("echostr")
	decryptedMes, err := serverGuard.decryptEchoStr(request, strEcho)
	bodyData := io.NopCloser(bytes.NewBufferString(decryptedMes))
	rs := &http.Response{
		Body:       bodyData,
		StatusCode: http.StatusOK,
	}

	logger.Info("Server response created:", "content", decryptedMes)

	return rs, err
}

// https://developer.work.weixin.qq.com/document/path/90930
func (serverGuard *ServerGuard) Serve(request *http.Request) (response *http.Response, err error) {
	logger := (*serverGuard.App).GetComponent("Logger").(*logger2.Logger)
	logger.Info("Request received:",
		"method", request.Method,
		"uri", request.URL,
		"content-type", request.Header.Get("Content-Type"),
		"content", request.Body,
	)

	validatedGuard, err := serverGuard.Validate(request)
	if err != nil {
		return nil, err
	}

	response, err = validatedGuard.Resolve(request)

	logger.Info("Server response created:", "content", response.ContentLength)

	return response, err
}

func (serverGuard *ServerGuard) validate(request *http.Request) (*ServerGuard, error) {
	if !serverGuard.alwaysValidate && serverGuard.IsSafeMode(request) {
		return serverGuard, nil
	}

	if request == nil {
		return nil, errors.New("request is invalid")
	}
	query := request.URL.Query()

	sign := serverGuard.signature([]string{
		serverGuard.GetToken(),
		query.Get("timestamp"),
		query.Get("nonce"),
	})

	if query.Get("signature") != sign {
		return serverGuard, errors.New("invalid request signature")
	}

	return serverGuard, nil
}

func (serverGuard *ServerGuard) GetEvent(request *http.Request) (callback *models.Callback, callbackHeader *models.CallbackMessageHeader, err error) {
	if request == nil {
		return nil, nil, errors.New("request is invalid")
	}
	var b []byte = []byte("")
	if request.Body != http.NoBody {
		b, err = io.ReadAll(request.Body)
		if err != nil || b == nil {
			return nil, nil, err
		}
	}

	// 请求数据类型
	rDataType := serverGuard.RequestDataType(request)

	callback, err = serverGuard.ParseMessage(string(b), rDataType)
	if err != nil {
		return nil, nil, err
	}

	if serverGuard.IsSafeMode(request) && callback.Encrypt != "" {
		callbackHeader, err = serverGuard.DecryptEvent(request, string(b))
	} else {
		callbackHeader = &models.CallbackMessageHeader{}
		if rDataType == messages.DataTypeXML {
			err = xml.Unmarshal(b, callbackHeader)
		} else {
			err = json.Unmarshal(b, callbackHeader)
		}
		callbackHeader.Content = b
	}

	return callback, callbackHeader, err
}

func (serverGuard *ServerGuard) GetMessage(request *http.Request) (callback *models.Callback, callbackHeader *models.CallbackMessageHeader, Decrypted interface{}, err error) {
	b := []byte("")
	if request.Body != http.NoBody {
		b, err = io.ReadAll(request.Body)
		if err != nil || b == nil {
			return nil, nil, nil, err
		}
	}

	callback, err = serverGuard.ParseMessage(string(b), serverGuard.RequestDataType(request))
	if err != nil {
		return nil, nil, nil, err
	}

	if serverGuard.IsSafeMode(request) && callback.Encrypt != "" {
		callbackHeader, Decrypted, err = serverGuard.decryptMessage(request, string(b))
	} else {
		callbackHeader = &models.CallbackMessageHeader{
			Text:       callback.Text,
			ToUserName: callback.ToUserName,
		}
	}

	return callback, callbackHeader, Decrypted, err
}

func (serverGuard *ServerGuard) ResolveEvent(request *http.Request, closure func(event contract.EventInterface) interface{}) (rs *http.Response, err error) {
	result, err := serverGuard.HandleEvent(request, closure)
	if err != nil {
		return nil, err
	}

	if serverGuard.ShouldReturnRawResponse(request) {
		resultRS := ""
		if (*result)["response"] != nil {
			resultRS = (*result)["response"].(string)
		}
		rs = &http.Response{
			Body: io.NopCloser(bytes.NewBufferString(resultRS)),
		}

	} else {
		strBuiltResponse := serverGuard.buildResponse(request, (*result)["to"].(string), (*result)["from"].(string), (*result)["response"])
		header := http.Header{}
		header.Set("Content-Type", "application/xml")
		rs = &http.Response{
			Body:       io.NopCloser(bytes.NewBufferString(strBuiltResponse)),
			StatusCode: http.StatusOK,
			Header:     header,
		}
	}

	return rs, nil
}

func (serverGuard *ServerGuard) OverrideResolve() {
	serverGuard.Resolve = func(request *http.Request) (rs *http.Response, err error) {
		result, err := serverGuard.handleRequest(request)
		if err != nil {
			return nil, err
		}

		if serverGuard.ShouldReturnRawResponse(request) {
			resultRS := ""
			if (*result)["response"] != nil {
				resultRS = (*result)["response"].(string)
			}
			rs = &http.Response{
				Body: io.NopCloser(bytes.NewBufferString(resultRS)),
			}

		} else {
			strBuiltResponse := serverGuard.buildResponse(request, (*result)["to"].(string), (*result)["from"].(string), (*result)["response"])
			header := http.Header{}
			header.Set("Content-Type", "application/xml")
			rs = &http.Response{
				Body:       io.NopCloser(bytes.NewBufferString(strBuiltResponse)),
				StatusCode: http.StatusOK,
				Header:     header,
			}
		}

		return rs, nil
	}
}

func (serverGuard *ServerGuard) OverrideGetToken() {
	serverGuard.GetToken = func() string {
		config := (*serverGuard.App).GetConfig()
		token := config.Get("token", "").(string)

		return token
	}
}

func (serverGuard *ServerGuard) buildResponse(request *http.Request, to string, from string, message interface{}) string {
	var toMessage contract.MessageInterface
	switch message := message.(type) {
	case nil:
		return SUCCESS_EMPTY_RESPONSE

	case string:
		if SUCCESS_EMPTY_RESPONSE == message {
			return SUCCESS_EMPTY_RESPONSE
		} else {
			toMessage = messages.NewText(message)
			break
		}
	case int:
		toMessage = messages.NewText(strconv.Itoa(message))
	case messages.Raw:
		return message.Get("content", SUCCESS_EMPTY_RESPONSE).(string)
	case []*object.HashMap:
		toMessage = messages.NewNews(message)
	case contract.MessageInterface:
		toMessage = message
	default:

	}

	return serverGuard.buildReply(request, to, from, toMessage)
}

func (serverGuard *ServerGuard) OverrideHandleEvent() {
	serverGuard.HandleEvent = func(r *http.Request, closure func(event contract.EventInterface) interface{}) (*object.HashMap, error) {
		_, msgHeader, err := serverGuard.GetEvent(r)
		if err != nil {
			return nil, err
		}

		fromUserName := ""
		toUserName := ""

		if msgHeader != nil {
			if msgHeader.MsgType != "" {
				fromUserName = msgHeader.FromUserName
				toUserName = msgHeader.ToUserName
			}
		}
		response := closure(msgHeader)

		return &object.HashMap{
			"to":       fromUserName,
			"from":     toUserName,
			"response": response,
		}, nil
	}
}

func (serverGuard *ServerGuard) handleRequest(request *http.Request) (*object.HashMap, error) {
	_, msgHeader, decryptedMessage, err := serverGuard.GetMessage(request)
	if err != nil {
		return nil, err
	}

	fromUserName := ""
	toUserName := ""

	messageType := "text"
	if msgHeader != nil {
		if msgHeader.MsgType != "" {
			messageType = msgHeader.MsgType
			fromUserName = msgHeader.FromUserName
			toUserName = msgHeader.ToUserName
		}
	}
	response := serverGuard.Dispatch(request, MESSAGE_TYPE_MAPPING[messageType], msgHeader, decryptedMessage)

	return &object.HashMap{
		"to":       fromUserName,
		"from":     toUserName,
		"response": response,
	}, nil
}

func (serverGuard *ServerGuard) buildReply(request *http.Request, to string, from string, message contract.MessageInterface) (response string) {
	prepends := &object.HashMap{
		"ToUserName":   to,
		"FromUserName": from,
		"CreateTime":   time.Now(),
		"MsgType":      message.GetType(),
	}
	transformedResponse, _ := message.TransformToXml(prepends, false)
	response = transformedResponse.(string)
	if serverGuard.IsSafeMode(request) {
		// tbd log here
		encryptor := (*serverGuard.App).GetComponent("Encryptor").(*Encryptor)
		encryptedResponse, err := encryptor.Encrypt(response, "", "")
		if err == nil {
			response = string(encryptedResponse)
		} else {
			// tbd log here
			println("encryptor encrypt message error: ", err.ErrMsg)
		}

	}

	return response
}

func (serverGuard *ServerGuard) signature(params []string) string {
	sort.Strings(params)
	strJoined := strings.Join(params, "")

	hash := sha1.New()
	hash.Write([]byte(strJoined))
	bs := hash.Sum(nil)

	return fmt.Sprintf("%x", bs)
}

func (serverGuard *ServerGuard) isSafeMode(request *http.Request) bool {
	query := request.URL.Query()

	return query.Get("signature") != "" && "aes" == query.Get("encrypt_type")
}

func (serverGuard *ServerGuard) requestDataType(request *http.Request) string {
	if strings.HasPrefix(request.Header.Get("Content-Type"), "text/xml") ||
		strings.HasPrefix(request.Header.Get("Content-Type"), "application/xml") {
		// xml 格式
		return messages.DataTypeXML
	} else {
		// json 格式
		return messages.DataTypeJSON
	}
}

func (serverGuard *ServerGuard) ParseMessage(content string, dataType string) (callback *models.Callback, err error) {
	callback = &models.Callback{}

	if len(content) <= 0 {
		return nil, errors.New("request body is empty")
	}

	if dataType == messages.DataTypeXML {
		err = xml.Unmarshal([]byte(content), callback)
		if err != nil {
			return nil, err
		}
	} else {
		err = object.JsonDecode([]byte(content), callback)
		if err != nil {
			return nil, err
		}
	}

	return callback, err
}

func (serverGuard *ServerGuard) parseMessage2(content string) (dataContent *object.HashMap, err error) {
	dataContent = nil
	if content != "" {
		// check xml format
		if content[0] == '<' {
			dataContent = &object.HashMap{}
			// err = xml.Unmarshal([]byte(content), &dataContent)
			*dataContent, err = object.Xml2Map([]byte(content))
			if err != nil {
				return nil, err
			}
		}
	}

	if dataContent == nil {
		// Handle JSON format.
		dataContent = &object.HashMap{}
		err = json.Unmarshal([]byte(content), &dataContent)
		if err != nil {
			return dataContent, nil
		}
	}

	return dataContent, err
}

func (serverGuard *ServerGuard) shouldReturnRawResponse(request *http.Request) bool {
	return false
}

func (serverGuard *ServerGuard) DecryptEvent(request *http.Request, content string) (callbackHeader *models.CallbackMessageHeader, err error) {
	encryptor := (*serverGuard.App).GetComponent("Encryptor").(*Encryptor)
	query := request.URL.Query()
	buf, cryptErr := encryptor.Decrypt(
		[]byte(content),
		query.Get("msg_signature"),
		query.Get("nonce"),
		query.Get("timestamp"),
	)
	if cryptErr != nil {
		return nil, errors.New(cryptErr.ErrMsg)
	}

	callbackHeader = &models.CallbackMessageHeader{}
	if serverGuard.RequestDataType(request) == messages.DataTypeXML {
		err = xml.Unmarshal(buf, callbackHeader)
	} else {
		err = json.Unmarshal(buf, callbackHeader)
	}
	if err != nil {
		return nil, err
	}

	callbackHeader.Content = buf

	return callbackHeader, err
}

func (serverGuard *ServerGuard) decryptEchoStr(request *http.Request, content string) (decryptMessage string, err error) {
	encryptor := (*serverGuard.App).GetComponent("Encryptor").(*Encryptor)
	query := request.URL.Query()
	buf, cryptErr := encryptor.VerifyUrl(
		content,
		query.Get("msg_signature"),
		query.Get("nonce"),
		query.Get("timestamp"),
	)
	if cryptErr != nil {
		return "", errors.New(cryptErr.ErrMsg)
	}

	return string(buf), err
}

func (serverGuard *ServerGuard) decryptMessage(request *http.Request, content string) (callbackHeader *models.CallbackMessageHeader, decryptMessage interface{}, err error) {
	encryptor := (*serverGuard.App).GetComponent("Encryptor").(*Encryptor)
	query := request.URL.Query()
	buf, cryptErr := encryptor.Decrypt(
		[]byte(content),
		query.Get("msg_signature"),
		query.Get("nonce"),
		query.Get("timestamp"),
	)
	if cryptErr != nil {
		return nil, nil, errors.New(cryptErr.ErrMsg)
	}

	callbackHeader = &models.CallbackMessageHeader{}
	if serverGuard.RequestDataType(request) == messages.DataTypeXML {
		err = xml.Unmarshal(buf, callbackHeader)
	} else {
		err = json.Unmarshal(buf, callbackHeader)
	}
	if err != nil {
		return nil, nil, err
	}

	decryptMessage, err = serverGuard.ToCallbackType(callbackHeader, buf)

	return callbackHeader, decryptMessage, err
}
