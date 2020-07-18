package naming_client

import (
	"encoding/json"
	"errors"
	"log"
	"reflect"
	"sync"
	"time"

	"github.com/nacos-group/nacos-sdk-go/clients/cache"
	"github.com/nacos-group/nacos-sdk-go/model"
	"github.com/nacos-group/nacos-sdk-go/utils"
)

type HostReactor struct {
	serviceInfoMap       cache.ConcurrentMap
	cacheDir             string
	updateThreadNum      int
	serviceProxy         NamingProxy
	pushReceiver         PushReceiver
	subCallback          SubscribeCallback
	updateTimeMap        cache.ConcurrentMap
	updateCacheWhenEmpty bool
	lock                 *sync.Mutex
}

const Default_Update_Thread_Num = 20

func NewHostReactor(serviceProxy NamingProxy, cacheDir string, updateThreadNum int, notLoadCacheAtStart bool, subCallback SubscribeCallback, updateCacheWhenEmpty bool) HostReactor {
	if updateThreadNum <= 0 {
		updateThreadNum = Default_Update_Thread_Num
	}
	hr := HostReactor{
		serviceProxy:         serviceProxy,
		cacheDir:             cacheDir,
		updateThreadNum:      updateThreadNum,
		serviceInfoMap:       cache.NewConcurrentMap(),
		subCallback:          subCallback,
		updateTimeMap:        cache.NewConcurrentMap(),
		updateCacheWhenEmpty: updateCacheWhenEmpty,
		lock:                 new(sync.Mutex),
	}
	pr := NewPushRecevier(&hr)
	hr.pushReceiver = *pr
	if !notLoadCacheAtStart {
		hr.loadCacheFromDisk()
	}
	go hr.asyncUpdateService()
	return hr
}

func (hr *HostReactor) loadCacheFromDisk() {
	serviceMap := cache.ReadServicesFromFile(hr.cacheDir)
	if serviceMap == nil || len(serviceMap) == 0 {
		return
	}
	for k, v := range serviceMap {
		hr.serviceInfoMap.Set(k, v)
	}
}

func (hr *HostReactor) ProcessServiceJson(result string) {
	service := utils.JsonToService(result)
	if service == nil {
		return
	}
	cacheKey := utils.GetServiceCacheKey(service.Name, service.Clusters)

	oldDomain, ok := hr.serviceInfoMap.Get(cacheKey)
	if ok && !hr.updateCacheWhenEmpty {
		//if instance list is empty,not to update cache
		if service.Hosts == nil || len(service.Hosts) == 0 {
			log.Printf("[ERROR]:do not have useful host, ignore it, name:%s \n", service.Name)
			return
		}
	}
	hr.updateTimeMap.Set(cacheKey, uint64(utils.CurrentMillis()))
	hr.serviceInfoMap.Set(cacheKey, *service)
	if !ok || ok && !reflect.DeepEqual(service.Hosts, oldDomain.(model.Service).Hosts) {
		if !ok {
			log.Println("[INFO] service not found in cache " + cacheKey)
		} else {
			log.Printf("[INFO] service key:%s was updated to:%s \n", cacheKey, utils.ToJsonString(service))
		}
		cache.WriteServicesToFile(*service, hr.cacheDir)
		hr.subCallback.ServiceChanged(service)
	}
}

func (hr *HostReactor) GetServiceInfo(serviceName string, clusters string) (model.Service, error) {
	key := utils.GetServiceCacheKey(serviceName, clusters)
	cacheService, ok := hr.serviceInfoMap.Get(key)
	if !ok {
		hr.updateServiceNow(serviceName, clusters, key)
		if cacheService, ok = hr.serviceInfoMap.Get(key); !ok {
			return model.Service{}, errors.New("get service info failed")
		}
	}

	return cacheService.(model.Service), nil
}

func (hr *HostReactor) GetAllServiceInfo(nameSpace, groupName string, pageNo, pageSize uint32) model.ServiceList {
	data := model.ServiceList{}
	result, err := hr.serviceProxy.GetAllServiceInfoList(nameSpace, groupName, pageNo, pageSize)
	if err != nil {
		log.Printf("[ERROR]:query all services info return error!nameSpace:%s groupName:%s pageNo:%d, pageSize:%d err:%s \n", nameSpace, groupName, pageNo, pageSize, err.Error())
		return data
	}
	if result == "" {
		log.Printf("[ERROR]:query all services info is empty!nameSpace:%s  groupName:%s pageNo:%d, pageSize:%d \n", nameSpace, groupName, pageNo, pageSize)
		return data
	}

	err = json.Unmarshal([]byte(result), &data)
	if err != nil {
		log.Printf("[ERROR]: the result of quering all services info json.Unmarshal error !nameSpace:%s groupName:%s pageNo:%d, pageSize:%d \n", nameSpace, groupName, pageNo, pageSize)
		return data
	}
	return data
}

func (hr *HostReactor) updateServiceNow(serviceName, clusters, key string) {
	hr.lock.Lock()
	if _, ok := hr.serviceInfoMap.Get(key); !ok {
		result, err := hr.serviceProxy.QueryList(serviceName, clusters, hr.pushReceiver.port, false)

		if err != nil {
			log.Printf("[ERROR]:query list return error!servieName:%s cluster:%s  err:%s \n", serviceName, clusters, err.Error())
			return
		}
		if result == "" {
			log.Printf("[ERROR]:query list is empty!servieName:%s cluster:%s \n", serviceName, clusters)
			return
		}
		hr.ProcessServiceJson(result)
	}
	hr.lock.Unlock()
}

func (hr *HostReactor) asyncUpdateService() {
	sema := utils.NewSemaphore(hr.updateThreadNum)
	for {
		for _, v := range hr.serviceInfoMap.Items() {
			service := v.(model.Service)
			lastRefTime, ok := hr.updateTimeMap.Get(utils.GetServiceCacheKey(service.Name, service.Clusters))
			if !ok {
				lastRefTime = uint64(0)
			}
			if uint64(utils.CurrentMillis())-lastRefTime.(uint64) > service.CacheMillis {
				sema.Acquire()
				go func() {
					hr.asyncUpdateServiceNow(service.Name, service.Clusters)
					sema.Release()
				}()
			}
		}
		time.Sleep(1 * time.Second)
	}
}

func (hr *HostReactor) asyncUpdateServiceNow(serviceName, clusters string) {
	result, err := hr.serviceProxy.QueryList(serviceName, clusters, hr.pushReceiver.port, false)

	if err != nil {
		log.Printf("[ERROR]:query list return error!servieName:%s cluster:%s  err:%s \n", serviceName, clusters, err.Error())
		return
	}
	if result == "" {
		log.Printf("[ERROR]:query list is empty!servieName:%s cluster:%s \n", serviceName, clusters)
		return
	}
	hr.ProcessServiceJson(result)
}
