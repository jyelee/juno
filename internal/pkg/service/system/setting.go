package system

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/douyu/jupiter/pkg/xlog"

	"github.com/douyu/juno/pkg/model/db"
	"github.com/douyu/juno/pkg/model/view"
	"github.com/jinzhu/gorm"
)

type (
	setting struct {
		db              *gorm.DB
		settingCacheMtx sync.RWMutex
		settingCache    map[string]string
		subscribers     map[string][]SubscribeCallback // 设置修改事件订阅
		subscribersMtx  sync.RWMutex
	}

	SubscribeCallback func(content string)
)

func newSetting(db *gorm.DB) *setting {
	ret := &setting{
		db:           db,
		settingCache: map[string]string{},
		subscribers:  map[string][]SubscribeCallback{},
	}

	go ret.intervalSync()

	return ret
}

// 间隔时间从数据库同步
func (s *setting) intervalSync() {
	for {
		settingRecords := make([]db.SystemConfig, 0)
		err := s.db.Find(&settingRecords).Error
		if err != nil {
			xlog.Error("query setting records failed", xlog.String("err", err.Error()))
			goto End
		}

		s.settingCacheMtx.Lock()
		for _, record := range settingRecords {
			s.settingCache[record.Name] = record.Content
		}
		s.settingCacheMtx.Unlock()

	End:
		time.Sleep(30 * time.Second)
	}
}

//GetAll 从数据库获取所有设置
func (s *setting) GetAll() (settings map[string]string, err error) {
	settings = make(map[string]string)

	settingRecords := make([]db.SystemConfig, 0)
	err = s.db.Find(&settingRecords).Error
	if err != nil {
		return
	}

	for _, item := range settingRecords {
		// 判断配置名称是否有效
		if view.CheckSettingNameValid(string(item.Name)) {
			settings[string(item.Name)] = item.Content
		}
	}

	for field, config := range view.SettingFieldConfigs {
		// 如果为空，则使用默认值
		if _, ok := settings[field]; !ok {
			settings[field] = config.Default
		}
	}

	return
}

// Get 带缓存的设置获取
func (s *setting) Get(name string) (val string, err error) {
	// 先从内存中查
	{
		s.settingCacheMtx.RLock()
		val, ok := s.settingCache[name]
		s.settingCacheMtx.RUnlock()

		if ok {
			return val, nil
		}
	}

	// 如果没有从数据库查
	val, err = s.get(name)
	if err != nil {
		return val, err
	}

	{
		s.settingCacheMtx.Lock()
		s.settingCache[name] = val
		s.settingCacheMtx.Unlock()
	}

	return
}

// Create setting
func (s *setting) Create(name string, value string) (err error) {
	setting := db.SystemConfig{}
	err = s.db.Where("name = ?", name).First(&setting).Error
	if err != nil && !gorm.IsRecordNotFoundError(err) {
		return
	}

	setting.Name = string(name)
	setting.Content = value

	if err != nil && gorm.IsRecordNotFoundError(err) {
		err = s.db.Create(&setting).Error
		{
			s.settingCacheMtx.Lock()
			s.settingCache[name] = value
			s.settingCacheMtx.Unlock()
		}
		s.pubEvent(name, value)
	}
	return
}

// Set 设置系统设置
func (s *setting) Set(name string, value string) (err error) {
	tx := s.db.Begin()

	setting := db.SystemConfig{}
	query := tx.Where("name = ?", name).First(&setting)
	err = query.Error
	if err != nil && !gorm.IsRecordNotFoundError(err) {
		return
	}

	setting.Name = string(name)
	setting.Content = value

	if err != nil && gorm.IsRecordNotFoundError(err) {
		err = tx.Create(&setting).Error
	} else {
		err = tx.Save(&setting).Error
	}
	if err != nil {
		tx.Rollback()
		return err
	}

	err = tx.Commit().Error
	if err != nil {
		tx.Rollback()
		return err
	}

	{
		s.settingCacheMtx.Lock()
		s.settingCache[name] = value
		s.settingCacheMtx.Unlock()
	}

	s.pubEvent(name, value)

	return
}

// 从数据库获取
func (s *setting) get(name string) (val string, err error) {
	setting := db.SystemConfig{}
	err = s.db.Where("name = ?", name).First(&setting).Error
	if err != nil {
		if gorm.IsRecordNotFoundError(err) {
			if config, ok := view.SettingFieldConfigs[name]; ok {
				return config.Default, nil
			}

			return "", nil
		}

		return
	}

	val = setting.Content
	return
}

func (s *setting) pubEvent(name string, value string) {
	s.subscribersMtx.RLock()
	defer s.subscribersMtx.RUnlock()

	for _, callback := range s.subscribers[name] {
		go callback(value)
	}
}

func (s *setting) Subscribe(name string, callback SubscribeCallback, callOnce bool) {
	if !view.CheckSettingNameValid(name) {
		return
	}

	s.subscribersMtx.Lock()
	defer s.subscribersMtx.Unlock()

	if _, ok := s.subscribers[name]; !ok {
		s.subscribers[name] = make([]SubscribeCallback, 0)
	}

	s.subscribers[name] = append(s.subscribers[name], callback)

	// 订阅时发布
	if callOnce {
		content, err := s.get(name)
		if err != nil {
			return
		}

		callback(content)
	}
}

func (s *setting) K8SClusterSetting() (cluster view.SettingK8SCluster, err error) {
	content, err := s.Get(view.K8SClusterSettingName)
	if err != nil {
		return
	}

	err = json.Unmarshal([]byte(content), &cluster)
	if err != nil {
		return
	}

	return
}
