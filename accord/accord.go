package accord

import (
	"os"
	"os/signal"
	"path"
	"sync"

	"github.com/beeker1121/goque"
	"github.com/sirupsen/logrus"
)

const (
	SyncFilename    = "sync.queue"
	HistoryFilename = "history.stack"
	StateFilename   = "state.db"
)

// Manager is where the majority of application specific logic should be stored and is generally
// where you can actually *use* Accord. The Accord process will call these Manager functions
// so that implementing code can make use of our synchronization system.
type Manager interface {
	// Process is called when a Message should be handled and processed. This can be called both from
	// locally created new messages or sent from a remote Accord server; this is indicated by the fromRemote
	// boolean. You should try to resolve all errors internally, returning an error will tell Accord to blow
	// up
	Process(msg *Message, fromRemote bool) error

	// ShouldProcess gives the Manager a chance to filter which Messages get passed to Process so as to resolve
	// synchronization conflicts
	ShouldProcess(msg Message, history *goque.Stack) bool
}

// Accord is the main struct responsible for maintaining state and coordinating
// all goroutines that serve for synchronizing operations
type Accord struct {

	// The logrus logger to use for outputting logs. This should be passed in
	// so that the user has fine control over how exactly data gets logged (log output, log level,
	// etc...)
	Logger *logrus.Entry

	// Path to the directory where data should be stored. This should be passed in
	// so that the user can choose where the data ges stored
	dataDir string

	// The Manager implmentation that should be used for domain specific logic. This should be passed in
	// so that the user can add application specific logic
	manager Manager

	// A list of Components that should be started and processed by Accord. This should be passed in so
	// that the implementor can choose what kind of synchronization strategies to use (or write his/her own)
	components []Component

	// syncQueue is used to keep track of all of the messages that need to be synchronized
	// remotely
	syncQueue *goque.Queue

	// historyStack is used to keep track of the messages that were performed locally by this instance that
	// can be used for resolving merge conflicts
	historyStack *goque.Stack

	// state is used to keep track of the internal state of our process so help detect divergence
	// with other Accord processes. It's probably a bit of overkill to use a LevelDB database to keep
	// track of our state but it's the easiest way of creating a persisted, thread safe piece of data.
	// We're already using LevelDB for goque (which is why we're not going with Bolt) and it's very
	// possible we'll want to keep track of more advanced data for our state, which this will support
	state *State

	// shutdown is a channel that can be used to communicate to the Accord process from a goroutine that
	// it should shutdown. This will generally be used by Components when they encounter an unrecoverable
	// error and the only logical course of action is to shutdown the entire application
	shutdown chan error

	// signalChannel is used to detect when a signal comes in from the operating system
	signalChannel chan os.Signal

	// We need to make sure that we don't process more than one message at a time or else our state might
	// get messed up
	processMutex *sync.Mutex
}

// NewAccord creates a new instance of Accord for you to use. This function accepts an implementation
// of the Manager interface, which will be called upon to do application specific logic. A list of
// Components, so that the user what kind of synchronization strategies to use (or write his/her own).
// The path to the directory where Accord should store its data. And a Logrus entry, so that the user
// has fine control over how exactly logs get executed (log output, log level, hooks, etc...)
func NewAccord(manager Manager, components []Component, dataDir string, logger *logrus.Entry) *Accord {
	return &Accord{
		Logger:     logger,
		dataDir:    dataDir,
		manager:    manager,
		components: components,
	}
}

// Start prepares the Accord struct and then starts up its processes. We
// return an error if there was a problem beginning the processes or creating the
// environment, otherwise we return nil.
//
// Accord does the majority of it's work using goroutines in the background, so while
// this function starts them it will return immediately; meaning this method should
// be used in tandem with something like "Listen" to keep the process alive and Accord
// processing. (These two functions are kept separate so that the user has the option
// of taking more fine grained control over their process loop. In general however,
// under normal circumstances, you'll most likely always want to follow every call to
// Start with a call to Listen which is why we offer the StartAndListen to wrap the two
// together)
func (accord *Accord) Start(signals ...os.Signal) (err error) {
	accord.Logger.Info("Initializing Accord")

	// Our first course of action should be to setup our interrupt signals, so that
	// if one comes in during our setup process we don't get stopped in the middle
	accord.signalChannel = make(chan os.Signal, 1)
	if len(signals) > 0 {
		accord.Logger.WithField("signals", signals).Info("Registering shutdown signals")
		accord.signalChannel = make(chan os.Signal, 1)
		signal.Notify(accord.signalChannel, signals...)
	}

	// Setup our internal variables and components
	accord.processMutex = &sync.Mutex{}

	accord.syncQueue, err = goque.OpenQueue(path.Join(accord.dataDir, SyncFilename))
	if err != nil {
		accord.Logger.WithError(err).Error("Unable to load synchronization queue")
		return err
	}

	accord.historyStack, err = goque.OpenStack(path.Join(accord.dataDir, HistoryFilename))
	if err != nil {
		accord.Logger.WithError(err).Error("Unable to load history stack")
		return err
	}

	accord.state, err = OpenState(path.Join(accord.dataDir, StateFilename))
	if err != nil {
		accord.Logger.WithError(err).Error("Unable to load state")
		return err
	}

	accord.shutdown = make(chan error)

	accord.Logger.Info("Starting components")
	// Iterate over all of our passed in components and start them up one by one
	for _, comp := range accord.components {
		err := comp.Start(accord)
		if err != nil {
			return err
		}
	}

	return
}

// Stop safely closes down the components registered with Accord and waits for them to
// finish. This should *not* be used by components for closing Accord. Instead please use
// Shutdown
func (accord *Accord) Stop() {
	accord.Logger.Info("Stopping components")
	for _, comp := range accord.components {
		comp.Stop(0)
	}

	accord.Logger.Info("Waiting for components to stop")
	for _, comp := range accord.components {
		comp.WaitForStop()
	}

	accord.Logger.Info("Closing disk connections")
	accord.syncQueue.Close()
	accord.historyStack.Close()
	accord.state.Close()
}

// Listen simply listens on our interrupt channels and hangs until one comes in. If one does,
// the Accord process is closed down cleanly
func (accord *Accord) Listen() error {
	select {
	case <-accord.signalChannel:
		accord.Logger.Info("Received OS signal")
		accord.Stop()
		return nil

	case err := <-accord.shutdown:
		accord.Logger.WithError(err).Warn("Shutting down due to error")
		accord.Stop()
		return err
	}
}

// Shutdown provides a mechanism from which components and goroutines can trigger a shutdown
// of Accord if they are in an unrecoverable state.
func (accord *Accord) Shutdown(err error) {
	accord.shutdown <- err
}

// StartAndListen is a wrapper around the Init and Start functions, allowing for
// the user to completely begin the process with one function call
func (accord *Accord) StartAndListen(signals ...os.Signal) error {
	err := accord.Start(signals...)
	if err != nil {
		return err
	}

	err = accord.Listen()
	if err != nil {
		return err
	}

	return nil
}

// HandleNewMessage processes a newly created message and adds it to our queue to be
// synchronized
func (accord *Accord) HandleNewMessage(msg *Message) error {
	accord.processMutex.Lock()
	defer accord.processMutex.Unlock()

	accord.Logger.Debug("Processing a new message")
	err := accord.manager.Process(msg, false)
	if err != nil {
		accord.Logger.WithError(err).Warn("The manager had an error while processing a message. The safest thing to do is to blow ourselves up")
		accord.Shutdown(err)
		return err
	}

	err = accord.state.Update(msg)
	if err != nil {
		accord.Logger.WithError(err).Warn("We could not update our internal state. Blowing up our application")
		accord.Shutdown(err)
		return err
	}

	return nil
}
