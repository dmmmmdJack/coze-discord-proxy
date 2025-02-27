package discord

import (
	"coze-discord-proxy/common"
	"fmt"
	"sync"
	"time"
)

var (
	channelTimers sync.Map // 用于存储频道ID和对应的定时器
)

// SetChannelDeleteTimer 设置或重置频道的删除定时器
func SetChannelDeleteTimer(channelId string, duration time.Duration) {
	// 检查是否已存在定时器
	if timer, ok := channelTimers.Load(channelId); ok {
		timer.(*time.Timer).Stop() // 停止现有定时器
	}

	// 设置新的定时器
	newTimer := time.AfterFunc(duration, func() {
		ChannelDel(channelId)
		channelTimers.Delete(channelId) // 删除完成后从map中移除
	})

	// 存储新的定时器
	channelTimers.Store(channelId, newTimer)
}

// CancelChannelDeleteTimer 取消频道的删除定时器
func CancelChannelDeleteTimer(channelId string) {
	// 尝试从映射中获取定时器
	if timer, ok := channelTimers.Load(channelId); ok {
		// 如果定时器存在，尝试停止它
		if timer.(*time.Timer).Stop() {
			// 定时器成功停止后，从映射中移除
			channelTimers.Delete(channelId)
		} else {
			common.SysError(fmt.Sprintf("定时器无法停止或已触发，频道可能已被删除:%s", channelId))
		}
	} else {
		common.SysError(fmt.Sprintf("频道无定时删除:%s", channelId))
	}
}
