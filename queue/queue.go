package queue

import (
	d "ECE49595_PROJECT/dock"
	"encoding/json"
	"errors"
	"fmt"
	// "os"
	"sync"
	"time"
	ssh "ECE49595_PROJECT/ssh"
	"github.com/go-redis/redis"
)
var (
	queue Queue //singleton implementation
	Queue_container_name string
	CurrentKey string
	masterWorker QueueWorker
	slaveWorkers []QueueWorker
	CondVarEmpty *sync.Cond
	CondVarAvailable *sync.Cond
	masterOnline bool
	workersOnline []bool
	masterID int
	maxSlaves int
 	queueLck *sync.Mutex
	UnableToDeleteRequests map[string]string

)



func initQueue(api_conn_options, ssh_serv_conn_options *redis.Options) bool{
	api_connection, err1 := makeQueueConnection(api_conn_options, API_Q_CLI)
	ssh_serv_connection, err2 := makeQueueConnection(ssh_serv_conn_options, SSH_Q_CLI)
	queueLck = &sync.Mutex{}
	CondVarEmpty = &sync.Cond{L:queueLck}
	CondVarAvailable = &sync.Cond{L:queueLck}
	queue = Queue{
		API_CLI:               api_connection,
		SSH_SERV_CLI:          ssh_serv_connection,
		CREATED:               time.Now(),
		ONLINE:                err1 == nil && err2 == nil,
		API_CONN_OPTIONS:      api_conn_options,
		SSH_SERV_CONN_OPTIONS: ssh_serv_conn_options,
	}
	return queue.ONLINE
}
func MakeQueue(api_conn_options, ssh_serv_conn_options *redis.Options) error{
	var make_err error
	if Queue_container_name != "" {
		d.StopOneContainer(Queue_container_name)
		d.RemoveOneContainer(Queue_container_name)
	}
	Queue_container_name, make_err =  d.CreateNewContainer(QUEUE_CONTAINER_IMAGE,QUEUE_CONTAINER_MACHINE_PORT, QUEUE_CONTAINER_PORT)
	if make_err != nil{
		fmt.Println("Could not make queue.",make_err)
		return make_err
	}
	if initQueue(api_conn_options, ssh_serv_conn_options){
		return nil
	}else{
		return errors.New("Could not init queue.")
	}
}

func makeQueueConnection(options *redis.Options, name string) (*redis.Client, error) {
	//begin client
	queueConn := redis.NewClient(options)
	//check if a container exists
	_, err := queueConn.Ping().Result()
	if err != nil {
		fmt.Println("Queue container could not be pinged.")
		return nil, err
	}
	return queueConn, nil
}

func CheckAlive() Queue {
	_, err1 := queue.API_CLI.Ping().Result()
	_, err2 := queue.SSH_SERV_CLI.Ping().Result()
	if err1 != nil || err2 != nil {
		queue.API_CLI.Close()
		queue.SSH_SERV_CLI.Close()
		MakeQueue(queue.API_CONN_OPTIONS, queue.SSH_SERV_CONN_OPTIONS)
	}
	return queue
}
func IsRunning()bool{
	_, err := queue.API_CLI.Ping().Result()
	return err != nil 

}
func QueueIsEmpty() bool {
	rslt, err := queue.API_CLI.Keys("*").Result()
	if err == nil {
		return false
	}
	return len(rslt) == 0
}

func ShutDownQueue(force bool) int {
	if !QueueIsEmpty() && !force {
		return QUEUE_SHUTDOWN_FAIL_QUEUE_NOT_EMPTY
	}
	if EmptyQueue() != nil {
		return QUEUE_SHUTDOWN_FAIL_COULD_NOT_EMPTY_QUEUE
	}
	queue.API_CLI.Close()
	queue.SSH_SERV_CLI.Close()

	shutDownCheck := (d.StopOneContainer(Queue_container_name) == nil)
	if d.RemoveOneContainer(Queue_container_name) != nil{
		fmt.Println("Queue container stopped not removed.")
	}

	if !shutDownCheck {
		return QUEUE_SHUTDOWN_UNSUCCESSFUL
	}
	return QUEUE_SHUT_DOWN_SUCCESSFUL
}

func RestartQueue(force bool) int {
	if !QueueIsEmpty() && !force {
		return QUEUE_RESTART_FAIL_QUEUE_NOT_EMPTY
	}
	if EmptyQueue() != nil {
		return QUEUE_RESTART_FAIL_COULD_NOT_EMPTY_QUEUE
	}

	err:= d.RestartContainer(Queue_container_name); if err!=nil{
		return QUEUE_RESTART_UNSUCCESSFUL
	}
	if !initQueue(queue.API_CONN_OPTIONS, queue.SSH_SERV_CONN_OPTIONS) {
		return QUEUE_RESTART_UNSUCCESSFUL
	}
	return QUEUE_RESTART_SUCCESSFUL
}

func EmptyQueue() error {
	return queue.SSH_SERV_CLI.Del("*").Err()
}

func AddRequestToQueue(key string, request Queue_Request) error {
	json_byte_request, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return queue.API_CLI.Set(key, json_byte_request, TTL).Err()
}

func RemoveRequestFromQueue(key string) error {
	return queue.API_CLI.Del(key).Err()
}

func GetRequestFromQueue(key string) ([]byte, error) {
	json_byte_request, err := queue.SSH_SERV_CLI.Get(key).Bytes()
	if err != nil {
		return []byte{}, err
	}
	return json_byte_request, nil
}

func InitWorker(worker *QueueWorker, _ID int, isMaster, front, back bool ){
	worker.CondVarEmpty= CondVarEmpty
	worker.CondVarAvailable = CondVarAvailable
	worker.ID = _ID
	worker.CREATED = time.Now()
	worker.SERVED = 0
	worker.MASTER = isMaster
	worker.ONLINE = true
	worker.Lck = queueLck
	if !front && back{
		worker.SIDE =  QUEUE_BACKEND_WORKER//backend
		worker.SSH_SERV_CLI = queue.SSH_SERV_CLI
		worker.API_CLI = nil
	}else if front && !back{
		worker.SIDE = QUEUE_FRONTEND_WORKER //frontend
		worker.API_CLI = queue.API_CLI
		worker.SSH_SERV_CLI =  nil
	}else if front && back{
		worker.SIDE = QUEUE_MASTER_WORKER //master
		worker.API_CLI = queue.API_CLI
		worker.SSH_SERV_CLI = queue.SSH_SERV_CLI
	}
}

//Right now the plan is to entertain one request at a time but open connections to the users via go routines. But this function will also be called by a goroutine which constantly checks
//the appearance of new requests and opens connections.

func parseRequestJSON(rqst []byte)(Queue_Request, error){
	var request Queue_Request
	err := json.Unmarshal([]byte(rqst), &request)
	if err != nil {
		return Queue_Request{}, err
	}
	return request, nil
}
func BeginWork(worker *QueueWorker) {
	if worker.MASTER{
		masterOnline = true
	}else{
		workersOnline[worker.ID-masterID] = true
	}
	worker.ONLINE = true
	switch worker.SIDE{
	case QUEUE_FRONTEND_WORKER:
		for{
			worker.Lck.Lock()
			// var recvd_reqst Queue_Request
			// var key string
			//Mark: Get me a []byte JSON or type Queue_Request object here somehow
			//NOTE: It should have a differntiating name to be passed in as the key for the below function

			worker.CondVarAvailable.Signal()
			worker.SERVED++
			worker.Lck.Unlock()
		}
	case QUEUE_BACKEND_WORKER:
		for{
			worker.Lck.Lock()
			if QueueIsEmpty(){
				worker.CondVarAvailable.Wait()	
			}
			ID, err := GetRequestIDNextInLine(worker)
			if err != nil || ID == "" {
				worker.Lck.Unlock()
				continue
			}
			rqst, err :=GetRequestFromQueue(ID)
			if err != nil{
				worker.Lck.Unlock()
				continue
			}
			if ssh.SendOutConnection(rqst) != nil{
				worker.Lck.Unlock()
				continue
			}
			if RemoveRequestFromQueue(ID) != nil{
				UnableToDeleteRequests[ID] = "BAD_REQUEST"
			}
			worker.SERVED++
			worker.Lck.Unlock()
		}
	case QUEUE_MASTER_WORKER:
		for{
			
		}	
	}
	worker.CondVarAvailable = nil
	worker.CondVarEmpty = nil
	worker.ID = 0
	worker.CREATED = time.Time{}
	worker.SERVED = 0
	worker.SIDE = 0
	worker.MASTER = false
	worker.ONLINE = false
	worker.Lck = nil
}


func AllWorkersOffline()bool{
	for i:=0 ; i< maxSlaves;i++{
		if workersOnline[i] == true{
			return false
		}
	}
	return true
}

//SOHAM: I don't know how the "rslt" list is sorted, so there is no gaurantee that the requests will be entertained on a FIFO, LIFO, or random basis.
func GetRequestIDNextInLine(worker *QueueWorker) (string, error){
	rslt, err := queue.SSH_SERV_CLI.Keys("*").Result()
	if err != nil{
		return "", err
	}
	if len(rslt) ==0{
		return "", errors.New("No requests in line.")
	}
	for _, ID := range rslt{
		_, check := UnableToDeleteRequests[ID]
		if check {
			continue
		}
		return ID, nil
	}
	return "", errors.New("No valid request found, all belong to Unable to Delete.")
}


func StartAllWorkers()bool{
	slaveWorkers = make([]QueueWorker,maxSlaves)
	workersOnline = make([]bool, maxSlaves)

	InitWorker(&masterWorker, masterID, true, true, true) //both front and back, master
	go BeginWork(&masterWorker)

	for i:=0; i<maxSlaves/2;i++ {
		InitWorker(&slaveWorkers[i], masterID+i+1, false, true, false) //front
		go BeginWork(&slaveWorkers[i])
	}

	for i:=maxSlaves/2; i<maxSlaves;i++ {
		InitWorker(&slaveWorkers[i], masterID+i+1, false, false, true) // back
		go BeginWork(&slaveWorkers[i])
	}
	time.Sleep(time.Second*10)
	return true
}

func BeginQueueOperation(api_conn_options, ssh_serv_conn_options *redis.Options, _maxSlaves, _masterID int ) error{
	if _maxSlaves <=0 || _masterID <=0{
		return errors.New("Wrong MaxSlaves or Master ID argument.")
	}
	maxSlaves = _maxSlaves + (_maxSlaves %2) //to ensure we have equal slaves on each side of the queue.
	masterID = _masterID
	d.InitDock()
	err :=MakeQueue(api_conn_options, ssh_serv_conn_options); if err != nil{
		return err
	}
	if !StartAllWorkers(){
		return errors.New("Could not start one or more workers.")
	}
	return nil
}
