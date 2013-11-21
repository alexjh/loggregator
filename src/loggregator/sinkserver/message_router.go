package sinkserver

import (
	"github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/loggregatorlib/cfcomponent/instrumentation"
	"github.com/cloudfoundry/loggregatorlib/logmessage"
	"loggregator/groupedsinks"
	"loggregator/sinks"
	"net/url"
	"sync"
)

type messageRouter struct {
	dumpBufferSize              int
	parsedMessageChan           chan *logmessage.Message
	sinkCloseChan               chan sinks.Sink
	sinkOpenChan                chan sinks.Sink
	dumpReceiverChan            chan dumpReceiver
	activeDumpSinksCounter      int
	activeWebsocketSinksCounter int
	activeSyslogSinksCounter    int
	logger                      *gosteno.Logger
	errorChannel                chan *logmessage.Message
	skipCertVerify              bool
	*sync.RWMutex
}

func NewMessageRouter(maxRetainedLogMessages int, logger *gosteno.Logger, skipCertVerify bool) *messageRouter {
	sinkCloseChan := make(chan sinks.Sink, 20)
	sinkOpenChan := make(chan sinks.Sink, 20)
	dumpReceiverChan := make(chan dumpReceiver)
	messageChannel := make(chan *logmessage.Message, 2048)

	return &messageRouter{
		logger:            logger,
		parsedMessageChan: messageChannel,
		sinkCloseChan:     sinkCloseChan,
		sinkOpenChan:      sinkOpenChan,
		dumpReceiverChan:  dumpReceiverChan,
		dumpBufferSize:    maxRetainedLogMessages,
		RWMutex:           &sync.RWMutex{},
		errorChannel:      make(chan *logmessage.Message, 10),
		skipCertVerify:    skipCertVerify}
}

func (messageRouter *messageRouter) Start() {
	activeSinks := groupedsinks.NewGroupedSinks()

	for {
		select {
		case dr := <-messageRouter.dumpReceiverChan:
			if sink := activeSinks.DumpFor(dr.appId); sink != nil {
				data := sink.Dump()
				for _, m := range data {
					dr.outputChannel <- m
				}
				close(dr.outputChannel)
			}
		case s := <-messageRouter.sinkOpenChan:
			messageRouter.registerSink(s, activeSinks)
		case s := <-messageRouter.sinkCloseChan:
			messageRouter.unregisterSink(s, activeSinks)
		case receivedMessage := <-messageRouter.errorChannel:
			appId := receivedMessage.GetLogMessage().GetAppId()

			messageRouter.logger.Debugf("MessageRouter:ErrorChannel: Searching for sinks with appId [%s].", appId)
			for _, s := range activeSinks.For(appId) {
				if s.ShouldReceiveErrors() {
					messageRouter.logger.Debugf("MessageRouter:ErrorChannel: Sending Message to channel %v for sinks targeting [%s].", s.Identifier(), appId)
					s.Channel() <- receivedMessage
				}
			}
			messageRouter.logger.Debugf("MessageRouter:ErrorChannel: Done sending error message.")
		case receivedMessage := <-messageRouter.parsedMessageChan:
			messageRouter.logger.Debugf("MessageRouter:ParsedMessageChan: Received %d bytes of data from agent listener.", receivedMessage.GetRawMessageLength())

			//drain management
			appId := receivedMessage.GetLogMessage().GetAppId()
			messageRouter.manageDrains(activeSinks, appId, receivedMessage.GetLogMessage().GetDrainUrls(), receivedMessage.GetLogMessage().GetSourceType())
			messageRouter.manageDumps(activeSinks, appId)

			//send to drains and sinks
			messageRouter.logger.Debugf("MessageRouter:ParsedMessageChan: Searching for sinks with appId [%s].", appId)
			for _, s := range activeSinks.For(appId) {
				messageRouter.logger.Debugf("MessageRouter:ParsedMessageChan: Sending Message to channel %v for sinks targeting [%s].", s.Identifier(), appId)
				s.Channel() <- receivedMessage
			}
			messageRouter.logger.Debugf("MessageRouter:ParsedMessageChan: Done sending message.")
		}
	}
}

func (messageRouter *messageRouter) registerDumpChan(appId string) <-chan *logmessage.Message {
	dumpChan := make(chan *logmessage.Message, messageRouter.dumpBufferSize)
	dr := dumpReceiver{appId: appId, outputChannel: dumpChan}
	messageRouter.dumpReceiverChan <- dr
	return dumpChan
}

func (messageRouter *messageRouter) registerSink(s sinks.Sink, activeSinks *groupedsinks.GroupedSinks) bool {
	messageRouter.Lock()
	defer messageRouter.Unlock()

	ok := activeSinks.Register(s)
	switch s.(type) {
	case *sinks.DumpSink:
		messageRouter.activeDumpSinksCounter++
	case *sinks.SyslogSink:
		messageRouter.activeSyslogSinksCounter++
	case *sinks.WebsocketSink:
		messageRouter.activeWebsocketSinksCounter++
		go messageRouter.dumpToSink(s, activeSinks)
	}
	messageRouter.logger.Infof("MessageRouter: Sink with channel %v requested. Opened it.", s.Channel())
	return ok
}

func (messageRouter *messageRouter) dumpToSink(sink sinks.Sink, activeSinks *groupedsinks.GroupedSinks) {
	var data []*logmessage.Message
	if dumpSink := activeSinks.DumpFor(sink.AppId()); dumpSink != nil {
		data = dumpSink.Dump()
		if len(data) > 20 {
			data = data[len(data)-20:]
		}
	}
	for _, message := range data {
		sink.Channel() <- message
	}
}

func (messageRouter *messageRouter) unregisterSink(s sinks.Sink, activeSinks *groupedsinks.GroupedSinks) {
	messageRouter.Lock()
	defer messageRouter.Unlock()

	activeSinks.Delete(s)
	close(s.Channel())
	switch s.(type) {
	case *sinks.DumpSink:
		messageRouter.activeDumpSinksCounter--
	case *sinks.SyslogSink:
		messageRouter.activeSyslogSinksCounter--
	case *sinks.WebsocketSink:
		messageRouter.activeWebsocketSinksCounter--
	}
	messageRouter.logger.Infof("MessageRouter: Sink with channel %v requested closing. Closed it.", s.Channel())
}

func (messageRouter *messageRouter) manageDumps(activeSinks *groupedsinks.GroupedSinks, appId string) {
	if activeSinks.DumpFor(appId) == nil {
		s := sinks.NewDumpSink(appId, messageRouter.dumpBufferSize, messageRouter.logger)

		ok := messageRouter.registerSink(s, activeSinks)

		if ok {
			go s.Run()
		}
	}
}

func (messageRouter *messageRouter) manageDrains(activeSinks *groupedsinks.GroupedSinks, appId string, drainUrls []string, sourceType logmessage.LogMessage_SourceType) {
	if sourceType != logmessage.LogMessage_WARDEN_CONTAINER {
		return
	}
	//delete all drains for app
	if len(drainUrls) == 0 {
		for _, sink := range activeSinks.DrainsFor(appId) {
			messageRouter.unregisterSink(sink, activeSinks)
		}
		return
	}

	//delete all drains that were not sent
	for _, sink := range activeSinks.DrainsFor(appId) {
		if contains(sink.Identifier(), drainUrls) {
			continue
		}
		messageRouter.unregisterSink(sink, activeSinks)
	}

	//add all drains that didn't exist
	for _, drainUrl := range drainUrls {
		if activeSinks.DrainFor(appId, drainUrl) == nil {
			dl, err := url.Parse(drainUrl)
			if err != nil {
				messageRouter.logger.Warnf("MessageRouter: Error when trying to parse syslog url %v. Requesting close. Err: %v", drainUrl, err)
				continue
			}
			sysLogger := sinks.NewSyslogWriter(dl.Scheme, dl.Host, appId, messageRouter.skipCertVerify)
			s := sinks.NewSyslogSink(appId, drainUrl, messageRouter.logger, sysLogger, messageRouter.errorChannel)
			ok := messageRouter.registerSink(s, activeSinks)
			if ok {
				go s.Run()
			}
		}
	}
}

func (messageRouter *messageRouter) metrics() []instrumentation.Metric {
	messageRouter.RLock()
	defer messageRouter.RUnlock()

	return []instrumentation.Metric{
		instrumentation.Metric{Name: "numberOfDumpSinks", Value: messageRouter.activeDumpSinksCounter},
		instrumentation.Metric{Name: "numberOfSyslogSinks", Value: messageRouter.activeSyslogSinksCounter},
		instrumentation.Metric{Name: "numberOfWebsocketSinks", Value: messageRouter.activeWebsocketSinksCounter},
	}
}

func (messageRouter *messageRouter) Emit() instrumentation.Context {
	return instrumentation.Context{
		Name:    "messageRouter",
		Metrics: messageRouter.metrics(),
	}
}
