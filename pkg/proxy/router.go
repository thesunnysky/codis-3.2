// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package proxy

import (
	"sync"
	"time"

	"github.com/thesunnysky/codis/pkg/models"
	"github.com/thesunnysky/codis/pkg/utils/errors"
	"github.com/thesunnysky/codis/pkg/utils/log"
	"github.com/thesunnysky/codis/pkg/utils/redis"
)

const MaxSlotNum = models.MaxSlotNum

type Router struct {
	mu sync.RWMutex

	pool struct {
		//后端共享连接池，主备的形式存在
		primary *sharedBackendConnPool
		replica *sharedBackendConnPool
	}
	slots [MaxSlotNum]Slot

	config *Config
	online bool
	closed bool
}

//proxy创建Router
//始化了Router中的两个sharedBackendConnPool的结构，
func NewRouter(config *Config) *Router {
	s := &Router{config: config}
	s.pool.primary = newSharedBackendConnPool(config, config.BackendPrimaryParallel)
	s.pool.replica = newSharedBackendConnPool(config, config.BackendReplicaParallel)
	for i := range s.slots {
		s.slots[i].id = i
		s.slots[i].method = &forwardSync{}
	}
	return s
}

func (s *Router) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.online = true
}

func (s *Router) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true

	for i := range s.slots {
		s.fillSlot(&models.Slot{Id: i}, false, nil)
	}
}

func (s *Router) GetSlots() []*models.Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	slots := make([]*models.Slot, MaxSlotNum)
	for i := range s.slots {
		slots[i] = s.slots[i].snapshot()
	}
	return slots
}

func (s *Router) GetSlot(id int) *models.Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id < 0 || id >= MaxSlotNum {
		return nil
	}
	slot := &s.slots[id]
	return slot.snapshot()
}

func (s *Router) HasSwitched() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.slots {
		if s.slots[i].switched {
			return true
		}
	}
	return false
}

var (
	ErrClosedRouter  = errors.New("use of closed router")
	ErrInvalidSlotId = errors.New("use of invalid slot id")
	ErrInvalidMethod = errors.New("use of invalid forwarder method")
)

//Router 调用用来fillSlot
func (s *Router) FillSlot(m *models.Slot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedRouter
	}
	if m.Id < 0 || m.Id >= MaxSlotNum {
		return ErrInvalidSlotId
	}
	var method forwardMethod
	switch m.ForwardMethod {
	default:
		return ErrInvalidMethod
	case models.ForwardSync:
		method = &forwardSync{}
	case models.ForwardSemiAsync:
		method = &forwardSemiAsync{}
	}
	s.fillSlot(m, false, method)
	return nil
}

func (s *Router) KeepAlive() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosedRouter
	}
	s.pool.primary.KeepAlive()
	s.pool.replica.KeepAlive()
	return nil
}

func (s *Router) isOnline() bool {
	return s.online && !s.closed
}

// 将某个request分发给各个具体的slot进行处理, 该方法有session的handleRequest（）方法中调用，
// 通过对request的key进行hash运算，得到该key所在的slot id，然后该请求由对应的slot处理
// 共有以下三个方法：
//method 1. 根据key进行转发
func (s *Router) dispatch(r *Request) error {
	hkey := getHashKey(r.Multi, r.OpStr)
	var id = Hash(hkey) % MaxSlotNum
	slot := &s.slots[id]
	//交由slot的forward()方法来处理请求
	return slot.forward(r, hkey)
}

//method 2. 将request发到指定slot
func (s *Router) dispatchSlot(r *Request, id int) error {
	if id < 0 || id >= MaxSlotNum {
		return ErrInvalidSlotId
	}
	slot := &s.slots[id]
	return slot.forward(r, nil)
}

//method 3. 将request转发到指定的redis服务器地址，如果找不到就返回false
func (s *Router) dispatchAddr(r *Request, addr string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if bc := s.pool.primary.Get(addr).BackendConn(r.Database, r.Seed16(), false); bc != nil {
		bc.PushBack(r)
		return true
	}
	if bc := s.pool.replica.Get(addr).BackendConn(r.Database, r.Seed16(), false); bc != nil {
		bc.PushBack(r)
		return true
	}
	return false
}

//Important
func (s *Router) fillSlot(m *models.Slot, switched bool, method forwardMethod) {
	slot := &s.slots[m.Id]
	slot.blockAndWait()

	//清空models.Slot里面的backendConn
	slot.backend.bc.Release()
	//for gc
	slot.backend.bc = nil
	slot.backend.id = 0
	slot.migrate.bc.Release()
	//for gc
	slot.migrate.bc = nil
	slot.migrate.id = 0
	for i := range slot.replicaGroups {
		for _, bc := range slot.replicaGroups[i] {
			//clear replica groups backendConn
			bc.Release()
		}
	}
	slot.replicaGroups = nil

	slot.switched = switched

	//set slot.backend.bc
	if addr := m.BackendAddr; len(addr) != 0 {
		//设置slot的backendConn
		//从sharedBackendConnPool尝试后去slot的backendConn，如果没有的话就创建一个Conn在放入pool中
		slot.backend.bc = s.pool.primary.Retain(addr)
		//设置slot的backend id
		slot.backend.id = m.BackendAddrGroupId
	}
	//set slot.migrate.bc
	if from := m.MigrateFrom; len(from) != 0 {
		//设置slot的migrate backendConn
		slot.migrate.bc = s.pool.primary.Retain(from)
		slot.migrate.id = m.MigrateFromGroupId
	}
	if !s.config.BackendPrimaryOnly {
		for i := range m.ReplicaGroups {
			var group []*sharedBackendConn
			for _, addr := range m.ReplicaGroups[i] {
				group = append(group, s.pool.replica.Retain(addr))
			}
			if len(group) == 0 {
				continue
			}
			slot.replicaGroups = append(slot.replicaGroups, group)
		}
	}
	if method != nil {
		slot.method = method
	}

	if !m.Locked {
		slot.unblock()
	}
	if !s.closed {
		if slot.migrate.bc != nil {
			//switched is always false
			if switched {
				log.Warnf("fill slot %04d, backend.addr = %s, migrate.from = %s, locked = %t, +switched",
					slot.id, slot.backend.bc.Addr(), slot.migrate.bc.Addr(), slot.lock.hold)
			} else {
				log.Warnf("fill slot %04d, backend.addr = %s, migrate.from = %s, locked = %t",
					slot.id, slot.backend.bc.Addr(), slot.migrate.bc.Addr(), slot.lock.hold)
			}
		} else {
			if switched {
				log.Warnf("fill slot %04d, backend.addr = %s, locked = %t, +switched",
					slot.id, slot.backend.bc.Addr(), slot.lock.hold)
			} else {
				log.Warnf("fill slot %04d, backend.addr = %s, locked = %t",
					slot.id, slot.backend.bc.Addr(), slot.lock.hold)
			}
		}
	}
}

func (s *Router) SwitchMasters(masters map[int]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedRouter
	}
	cache := &redis.InfoCache{
		Auth: s.config.ProductAuth, Timeout: time.Millisecond * 100,
	}
	for i := range s.slots {
		s.trySwitchMaster(i, masters, cache)
	}
	return nil
}

func (s *Router) trySwitchMaster(id int, masters map[int]string, cache *redis.InfoCache) {
	var switched bool
	var m = s.slots[id].snapshot()

	hasSameRunId := func(addr1, addr2 string) bool {
		if addr1 != addr2 {
			rid1 := cache.GetRunId(addr1)
			rid2 := cache.GetRunId(addr2)
			return rid1 != "" && rid1 == rid2
		}
		return true
	}

	if addr := masters[m.BackendAddrGroupId]; addr != "" {
		if !hasSameRunId(addr, m.BackendAddr) {
			m.BackendAddr, switched = addr, true
		}
	}
	if addr := masters[m.MigrateFromGroupId]; addr != "" {
		if !hasSameRunId(addr, m.MigrateFrom) {
			m.MigrateFrom, switched = addr, true
		}
	}
	if switched {
		s.fillSlot(m, true, nil)
	}
}
