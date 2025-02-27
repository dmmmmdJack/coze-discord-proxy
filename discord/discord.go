package discord

import (
	"bytes"
	"context"
	"coze-discord-proxy/common"
	"coze-discord-proxy/model"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"github.com/gin-gonic/gin"
	"github.com/h2non/filetype"
	"golang.org/x/net/proxy"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var BotToken = os.Getenv("BOT_TOKEN")
var CozeBotId = os.Getenv("COZE_BOT_ID")
var GuildId = os.Getenv("GUILD_ID")
var ChannelId = os.Getenv("CHANNEL_ID")
var ProxyUrl = os.Getenv("PROXY_URL")
var ChannelAutoDelTime = os.Getenv("CHANNEL_AUTO_DEL_TIME")

var BotConfigList []model.BotConfig

var RepliesChans = make(map[string]chan model.ReplyResp)
var RepliesOpenAIChans = make(map[string]chan model.OpenAIChatCompletionResponse)
var RepliesOpenAIImageChans = make(map[string]chan model.OpenAIImagesGenerationResponse)

var ReplyStopChans = make(map[string]chan model.ChannelStopChan)
var Session *discordgo.Session

func StartBot(ctx context.Context, token string) {
	var err error
	Session, err = discordgo.New("Bot " + token)

	if err != nil {
		common.FatalLog("error creating Discord session,", err)
		return
	}

	if ProxyUrl != "" {
		proxyParse, client, err := NewProxyClient(ProxyUrl)
		if err != nil {
			common.FatalLog("error creating proxy client,", err)
		}
		Session.Client = client
		Session.Dialer.Proxy = http.ProxyURL(proxyParse)
		common.SysLog("Proxy Set Success!")
	}
	// 注册消息处理函数
	Session.AddHandler(messageUpdate)

	// 打开websocket连接并开始监听
	err = Session.Open()
	if err != nil {
		common.FatalLog("error opening connection,", err)
		return
	}
	// 读取机器人配置文件
	loadBotConfig()
	// 验证docker配置文件
	checkEnvVariable()
	common.SysLog("Bot is now running. Enjoy It.")

	go scheduleDailyMessage()

	go func() {
		<-ctx.Done()
		if err := Session.Close(); err != nil {
			common.FatalLog("error closing Discord session,", err)
		}
	}()

	// 等待信号
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc
}

func checkEnvVariable() {
	if BotToken == "" {
		common.FatalLog("环境变量 BOT_TOKEN 未设置")
	}
	if GuildId == "" {
		common.FatalLog("环境变量 GUILD_ID 未设置")
	}
	if ChannelId == "" {
		common.FatalLog("环境变量 CHANNEL_ID 未设置")
	}
	if CozeBotId == "" {
		common.FatalLog("环境变量 COZE_BOT_ID 未设置")
	} else if Session.State.User.ID == CozeBotId {
		common.FatalLog("环境变量 COZE_BOT_ID 不可为当前服务 BOT_TOKEN 关联的 BOT_ID")
	}

	if ProxyUrl != "" {
		_, _, err := NewProxyClient(ProxyUrl)
		if err != nil {
			common.FatalLog("环境变量 PROXY_URL 设置有误")
		}
	}
	if ChannelAutoDelTime != "" {
		_, _err := strconv.Atoi(ChannelAutoDelTime)
		if _err != nil {
			common.FatalLog("环境变量 CHANNEL_AUTO_DEL_TIME 设置有误")
		}
	}
	common.SysLog("Environment variable check passed.")
}

func loadBotConfig() {
	// 检查文件是否存在
	_, err := os.Stat("config/bot_config.json")
	if err != nil {
		if !os.IsNotExist(err) {
			common.SysError("载入bot_config.json文件异常")
		}
		return
	}

	// 读取文件
	file, err := os.ReadFile("config/bot_config.json")
	if err != nil {
		common.FatalLog("error reading bot config file,", err)
	}
	if len(file) == 0 {
		return
	}

	// 解析JSON到结构体切片  并载入内存
	err = json.Unmarshal(file, &BotConfigList)
	if err != nil {
		common.FatalLog("Error parsing JSON:", err)
	}

	common.LogInfo(context.Background(), fmt.Sprintf("载入配置文件成功 BotConfigs: %+v", BotConfigList))
}

// messageUpdate handles the updated messages in Discord.
func messageUpdate(s *discordgo.Session, m *discordgo.MessageUpdate) {
	// 提前检查参考消息是否为 nil
	if m.ReferencedMessage == nil {
		return
	}

	// 尝试获取 stopChan
	stopChan, exists := ReplyStopChans[m.ReferencedMessage.ID]
	if !exists {
		return
	}

	// 如果作者为 nil 或消息来自 bot 本身,则发送停止信号
	if m.Author == nil || m.Author.ID == s.State.User.ID {
		ChannelDel(m.ChannelID)
		stopChan <- model.ChannelStopChan{
			Id: m.ChannelID,
		}
		return
	}

	// 检查消息是否是对 bot 的回复
	for _, mention := range m.Mentions {
		if mention.ID == s.State.User.ID {
			replyChan, exists := RepliesChans[m.ReferencedMessage.ID]
			if exists {
				reply := processMessage(m)
				replyChan <- reply
			} else {
				replyOpenAIChan, exists := RepliesOpenAIChans[m.ReferencedMessage.ID]
				if exists {
					reply := processMessageForOpenAI(m)
					replyOpenAIChan <- reply
				} else {
					replyOpenAIImageChan, exists := RepliesOpenAIImageChans[m.ReferencedMessage.ID]
					if exists {
						reply := processMessageForOpenAIImage(m)
						replyOpenAIImageChan <- reply
					} else {
						return
					}
				}
			}
			// data: {"id":"chatcmpl-8lho2xvdDFyBdFkRwWAcMpWWAgymJ","object":"chat.completion.chunk","created":1706380498,"model":"gpt-4-turbo-0613","system_fingerprint":null,"choices":[{"index":0,"delta":{"content":"？"},"logprobs":null,"finish_reason":null}]}
			// data :{"id":"1200873365351698694","object":"chat.completion.chunk","created":1706380922,"model":"COZE","choices":[{"index":0,"message":{"role":"assistant","content":"你好！有什么我可以帮您的吗？如果有任"},"logprobs":null,"finish_reason":"","delta":{"content":"吗？如果有任"}}],"usage":{"prompt_tokens":13,"completion_tokens":19,"total_tokens":32},"system_fingerprint":null}

			// 如果消息包含组件或嵌入,则发送停止信号
			if len(m.Message.Components) > 0 {
				replyOpenAIChan, exists := RepliesOpenAIChans[m.ReferencedMessage.ID]
				if exists {
					reply := processMessageForOpenAI(m)
					stopStr := "stop"
					reply.Choices[0].FinishReason = &stopStr
					replyOpenAIChan <- reply
				}

				if ChannelAutoDelTime != "" {
					delTime, _ := strconv.Atoi(ChannelAutoDelTime)
					if delTime == 0 {
						CancelChannelDeleteTimer(m.ChannelID)
					} else if delTime > 0 {
						// 删除该频道
						SetChannelDeleteTimer(m.ChannelID, time.Duration(delTime)*time.Second)
					}
				} else {
					// 删除该频道
					SetChannelDeleteTimer(m.ChannelID, 5*time.Second)
				}
				stopChan <- model.ChannelStopChan{
					Id: m.ChannelID,
				}
			}

			return
		}
	}
}

// processMessage 提取并处理消息内容及其嵌入元素
func processMessage(m *discordgo.MessageUpdate) model.ReplyResp {
	var embedUrls []string
	for _, embed := range m.Embeds {
		if embed.Image != nil {
			embedUrls = append(embedUrls, embed.Image.URL)
		}
	}

	return model.ReplyResp{
		Content:   m.Content,
		EmbedUrls: embedUrls,
	}
}

func processMessageForOpenAI(m *discordgo.MessageUpdate) model.OpenAIChatCompletionResponse {

	if len(m.Embeds) != 0 {
		for _, embed := range m.Embeds {
			if embed.Image != nil && !strings.Contains(m.Content, embed.Image.URL) {
				if m.Content != "" {
					m.Content += "\n"
				}
				m.Content += fmt.Sprintf("%s\n![Image](%s)", embed.Image.URL, embed.Image.URL)
			}
		}
	}

	promptTokens := common.CountTokens(m.ReferencedMessage.Content)
	completionTokens := common.CountTokens(m.Content)

	return model.OpenAIChatCompletionResponse{
		ID:      m.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "gpt-4-turbo",
		Choices: []model.OpenAIChoice{
			{
				Index: 0,
				Message: model.OpenAIMessage{
					Role:    "assistant",
					Content: m.Content,
				},
			},
		},
		Usage: model.OpenAIUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
}

func processMessageForOpenAIImage(m *discordgo.MessageUpdate) model.OpenAIImagesGenerationResponse {
	var response model.OpenAIImagesGenerationResponse

	if len(m.Embeds) != 0 {
		for _, embed := range m.Embeds {
			if embed.Image != nil && !strings.Contains(m.Content, embed.Image.URL) {
				if m.Content != "" {
					m.Content += "\n"
				}
				response.Data = append(response.Data, struct {
					URL string `json:"url"`
				}{URL: embed.Image.URL})
			}
		}
	}

	return model.OpenAIImagesGenerationResponse{
		Created: time.Now().Unix(),
		Data:    response.Data,
	}
}

func SendMessage(c *gin.Context, channelID, cozeBotId, message string) (*discordgo.Message, error) {
	var ctx context.Context
	if c == nil {
		ctx = context.Background()
	} else {
		ctx = c.Request.Context()
	}

	if Session == nil {
		common.LogError(ctx, "discord session is nil")
		return nil, fmt.Errorf("discord session not initialized")
	}

	var sentMsg *discordgo.Message

	content := fmt.Sprintf("%s <@%s>", message, cozeBotId)

	if runeCount := len([]rune(content)); runeCount > 50000 {
		common.LogError(ctx, fmt.Sprintf("prompt已超过限制,请分段发送 [%v] %s", runeCount, content))
		return nil, fmt.Errorf("prompt已超过限制,请分段发送 [%v]", runeCount)
	}

	// 特殊处理
	content = strings.ReplaceAll(content, "\\n", " \\n ")

	for i, msg := range common.ReverseSegment(content, 2000) {
		sentMsg, err := Session.ChannelMessageSend(channelID, msg)
		if err != nil {
			common.LogError(ctx, fmt.Sprintf("error sending message: %s", err))
			return nil, fmt.Errorf("error sending message")
		}
		if i == len(common.ReverseSegment(content, 2000))-1 {
			return sentMsg, nil
		}
	}
	return sentMsg, nil
}

func ChannelCreate(guildID, channelName string, channelType int) (string, error) {
	// 创建新的频道
	st, err := Session.GuildChannelCreate(guildID, channelName, discordgo.ChannelType(channelType))
	if err != nil {
		common.LogError(context.Background(), fmt.Sprintf("创建频道时异常 %s", err.Error()))
		return "", err
	}
	return st.ID, nil
}

func ChannelDel(channelId string) (string, error) {
	// 删除频道
	st, err := Session.ChannelDelete(channelId)
	if err != nil {
		common.LogError(context.Background(), fmt.Sprintf("删除频道时异常 %s", err.Error()))
		return "", err
	}
	return st.ID, nil
}

func ChannelCreateComplex(guildID, parentId, channelName string, channelType int) (string, error) {
	// 创建新的子频道
	st, err := Session.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:     channelName,
		Type:     discordgo.ChannelType(channelType),
		ParentID: parentId,
	})
	if err != nil {
		common.LogError(context.Background(), fmt.Sprintf("创建子频道时异常 %s", err.Error()))
		return "", err
	}
	return st.ID, nil
}

func ThreadStart(channelId, threadName string, archiveDuration int) (string, error) {
	// 创建新的线程
	th, err := Session.ThreadStart(channelId, threadName, discordgo.ChannelTypeGuildText, archiveDuration)

	if err != nil {
		common.LogError(context.Background(), fmt.Sprintf("创建线程时异常 %s", err.Error()))
		return "", err
	}
	return th.ID, nil
}

func NewProxyClient(proxyUrl string) (proxyParse *url.URL, client *http.Client, err error) {

	proxyParse, err = url.Parse(proxyUrl)
	if err != nil {
		common.FatalLog("代理地址设置有误")
	}

	if strings.HasPrefix(proxyParse.Scheme, "http") {
		httpTransport := &http.Transport{
			Proxy: http.ProxyURL(proxyParse),
		}
		return proxyParse, &http.Client{
			Transport: httpTransport,
		}, nil
	} else if strings.HasPrefix(proxyParse.Scheme, "sock") {
		dialer, err := proxy.SOCKS5("tcp", proxyParse.Host, nil, proxy.Direct)
		if err != nil {
			log.Fatal("Error creating dialer, ", err)
		}

		dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}

		// 使用该拨号器创建一个 HTTP 客户端
		httpClient := &http.Client{
			Transport: &http.Transport{
				DialContext: dialContext,
			},
		}

		return proxyParse, httpClient, nil
	} else {
		return nil, nil, fmt.Errorf("仅支持sock和http代理！")
	}

}

func scheduleDailyMessage() {
	for {
		// 计算距离下一个晚上12点的时间间隔
		now := time.Now()
		next := now.Add(time.Hour * 24)
		next = time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, next.Location())
		delay := next.Sub(now)

		// 等待直到下一个间隔
		time.Sleep(delay)

		var taskBotConfigs = BotConfigList

		taskBotConfigs = append(taskBotConfigs, model.BotConfig{
			ChannelId: ChannelId,
			CozeBotId: CozeBotId,
		})

		taskBotConfigs = model.FilterUniqueBotChannel(taskBotConfigs)

		common.SysLog("CDP Scheduled Task Job Start!")
		var sendChannelList []string
		for _, config := range taskBotConfigs {
			var sendChannelId string
			if config.ChannelId == "" {
				nextID, _ := common.NextID()
				sendChannelId, _ = ChannelCreate(GuildId, fmt.Sprintf("对话%s", nextID), 0)
				sendChannelList = append(sendChannelList, sendChannelId)
			} else {
				sendChannelId = config.ChannelId
			}
			_, err := SendMessage(nil, sendChannelId, config.CozeBotId, "CDP Scheduled Task Job Send Msg Success！")
			if err != nil {
				common.SysError(fmt.Sprintf("ChannelId{%s} BotId{%s} 活跃机器人任务消息发送异常!", sendChannelId, config.CozeBotId))
			} else {
				common.SysLog(fmt.Sprintf("ChannelId{%s} BotId{%s} 活跃机器人任务消息发送成功!", sendChannelId, config.CozeBotId))
			}
			time.Sleep(5 * time.Second)
		}
		for _, channelId := range sendChannelList {
			ChannelDel(channelId)
		}
		common.SysLog("CDP Scheduled Task Job End!")

	}
}

func UploadToDiscordAndGetURL(channelID string, base64Data string) (string, error) {

	// 获取";base64,"后的Base64编码部分
	dataParts := strings.Split(base64Data, ";base64,")
	if len(dataParts) != 2 {
		return "", fmt.Errorf("")
	}
	base64Data = dataParts[1]

	data, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return "", err
	}
	// 创建一个新的文件读取器
	file := bytes.NewReader(data)

	kind, err := filetype.Match(data)

	if err != nil {
		return "", fmt.Errorf("无法识别的文件类型")
	}

	// 创建一个新的 MessageSend 结构
	m := &discordgo.MessageSend{
		Files: []*discordgo.File{
			{
				Name:   fmt.Sprintf("image-%s.%s", common.GetTimeString(), kind.Extension),
				Reader: file,
			},
		},
	}

	// 发送消息
	message, err := Session.ChannelMessageSendComplex(channelID, m)
	if err != nil {
		return "", err
	}

	// 检查消息中是否包含附件,并获取 URL
	if len(message.Attachments) > 0 {
		return message.Attachments[0].URL, nil
	}

	return "", fmt.Errorf("no attachment found in the message")
}

// FilterConfigs 根据proxySecret和channelId过滤BotConfig
func FilterConfigs(configs []model.BotConfig, secret string, channelId *string) []model.BotConfig {
	var filteredConfigs []model.BotConfig
	for _, config := range configs {
		matchSecret := secret == "" || config.ProxySecret == secret
		matchChannelId := channelId == nil || *channelId == "" || config.ChannelId == *channelId
		if matchSecret && matchChannelId {
			filteredConfigs = append(filteredConfigs, config)
		}
	}
	return filteredConfigs
}
