//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/coocood/freecache"
	"github.com/duke-git/lancet/v2/convertor"
	"github.com/duke-git/lancet/v2/mathutil"
	"github.com/duke-git/lancet/v2/slice"
	"github.com/duke-git/lancet/v2/strutil"
	"github.com/energye/systray"
	"github.com/go-resty/resty/v2"
	"github.com/go-toast/toast"
	"github.com/robfig/cron/v3"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/runtime"
	"go-stock/backend/data"
	"go-stock/backend/db"
	"go-stock/backend/logger"
	"go-stock/backend/models"
	"golang.org/x/sys/windows/registry"
	"os"
	"strings"
	"time"
)

// App struct
type App struct {
	ctx        context.Context
	cache      *freecache.Cache
	cron       *cron.Cron
	cronEntrys map[string]cron.EntryID
}

// NewApp creates a new App application struct
func NewApp() *App {
	cacheSize := 512 * 1024
	cache := freecache.NewCache(cacheSize)
	c := cron.New(cron.WithSeconds())
	c.Start()
	return &App{
		cache:      cache,
		cron:       c,
		cronEntrys: make(map[string]cron.EntryID),
	}
}

// startup is called at application startup
func (a *App) startup(ctx context.Context) {
	defer PanicHandler()
	runtime.EventsOn(ctx, "frontendError", func(optionalData ...interface{}) {
		logger.SugaredLogger.Errorf("Frontend error: %v\n", optionalData)
	})
	logger.SugaredLogger.Infof("Version:%s", Version)
	// Perform your setup here
	a.ctx = ctx

	// 创建系统托盘
	//systray.RunWithExternalLoop(func() {
	//	onReady(a)
	//}, func() {
	//	onExit(a)
	//})

	systray.Run(func() {
		onReady(a)
	}, func() {
		onExit(a)
	})
}

func (a *App) CheckUpdate() {
	releaseVersion := &models.GitHubReleaseVersion{}
	_, err := resty.New().R().
		SetResult(releaseVersion).
		Get("https://api.github.com/repos/ArvinLovegood/go-stock/releases/latest")
	if err != nil {
		logger.SugaredLogger.Errorf("get github release version error:%s", err.Error())
		return
	}
	logger.SugaredLogger.Infof("releaseVersion:%+v", releaseVersion.TagName)
	if releaseVersion.TagName != Version {
		tag := &models.Tag{}
		_, err = resty.New().R().
			SetResult(tag).
			Get("https://api.github.com/repos/ArvinLovegood/go-stock/git/ref/tags/" + releaseVersion.TagName)
		if err == nil {
			releaseVersion.Tag = *tag
		}
		commit := &models.Commit{}
		_, err = resty.New().R().
			SetResult(commit).
			Get(tag.Object.Url)
		if err == nil {
			releaseVersion.Commit = *commit
		}

		go runtime.EventsEmit(a.ctx, "updateVersion", releaseVersion)
	}
}

// domReady is called after front-end resources have been loaded
func (a *App) domReady(ctx context.Context) {
	defer PanicHandler()

	// Add your action here
	//定时更新数据
	config := data.NewSettingsApi(&data.Settings{}).GetConfig()
	go func() {
		interval := config.RefreshInterval
		if interval <= 0 {
			interval = 1
		}
		ticker := time.NewTicker(time.Second * time.Duration(interval))
		defer ticker.Stop()
		for range ticker.C {
			MonitorStockPrices(a)
		}
	}()

	//刷新基金净值信息
	go func() {
		ticker := time.NewTicker(time.Second * time.Duration(60))
		defer ticker.Stop()
		for range ticker.C {
			MonitorFundPrices(a)
		}
	}()

	if config.EnableNews {
		go func() {
			ticker := time.NewTicker(time.Second * time.Duration(60))
			defer ticker.Stop()
			for range ticker.C {
				telegraph := refreshTelegraphList()
				if telegraph != nil {
					go runtime.EventsEmit(a.ctx, "telegraph", telegraph)
				}
			}

		}()
		go runtime.EventsEmit(a.ctx, "telegraph", refreshTelegraphList())
	}
	go MonitorStockPrices(a)
	go MonitorFundPrices(a)
	go data.NewFundApi().AllFund()
	//检查新版本
	go func() {
		a.CheckUpdate()
		a.cron.AddFunc("30 05 8,12,20 * * *", func() {
			logger.SugaredLogger.Errorf("Checking for updates...")
			a.CheckUpdate()
		})

	}()

	//检查谷歌浏览器
	//go func() {
	//	f := checkChromeOnWindows()
	//	if !f {
	//		go runtime.EventsEmit(a.ctx, "warnMsg", "谷歌浏览器未安装,ai分析功能可能无法使用")
	//	}
	//}()

	//检查Edge浏览器
	//go func() {
	//	path, e := checkEdgeOnWindows()
	//	if !e {
	//		go runtime.EventsEmit(a.ctx, "warnMsg", "Edge浏览器未安装,ai分析功能可能无法使用")
	//	} else {
	//		logger.SugaredLogger.Infof("Edge浏览器已安装，路径为: %s", path)
	//	}
	//}()
	followList := data.NewStockDataApi().GetFollowList()
	for _, follow := range *followList {
		if follow.Cron == "" {
			continue
		}
		entryID, err := a.cron.AddFunc(follow.Cron, a.AddCronTask(follow))
		logger.SugaredLogger.Errorf("添加自动分析任务:%s cron=%s entryID:%v", follow.Name, follow.Cron, entryID)
		a.cronEntrys[follow.StockCode] = entryID
		if err != nil {
			return
		}
	}

}

func (a *App) AddCronTask(follow data.FollowedStock) func() {
	return func() {
		go runtime.EventsEmit(a.ctx, "warnMsg", "开始自动分析"+follow.Name+"_"+follow.StockCode)
		ai := data.NewDeepSeekOpenAi(a.ctx)
		msgs := ai.NewChatStream(follow.Name, follow.StockCode, "", nil)
		var res strings.Builder

		chatId := ""
		question := ""
		for msg := range msgs {
			if msg["extraContent"] != nil {
				res.WriteString(msg["extraContent"].(string) + "\n")
			}
			if msg["content"] != nil {
				res.WriteString(msg["content"].(string))
			}
			if msg["chatId"] != nil {
				chatId = msg["chatId"].(string)
			}
			if msg["question"] != nil {
				question = msg["question"].(string)
			}
		}
		data.NewDeepSeekOpenAi(a.ctx).SaveAIResponseResult(follow.StockCode, follow.Name, res.String(), chatId, question)
		go runtime.EventsEmit(a.ctx, "warnMsg", "AI分析完成："+follow.Name+"_"+follow.StockCode)

	}
}

func refreshTelegraphList() *[]string {
	url := "https://www.cls.cn/telegraph"
	response, err := resty.New().R().
		SetHeader("Referer", "https://www.cls.cn/").
		SetHeader("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/117.0.0.0 Safari/537.36 Edg/117.0.2045.60").
		Get(fmt.Sprintf(url))
	if err != nil {
		return &[]string{}
	}
	//logger.SugaredLogger.Info(string(response.Body()))
	document, err := goquery.NewDocumentFromReader(strings.NewReader(string(response.Body())))
	if err != nil {
		return &[]string{}
	}
	var telegraph []string
	document.Find("div.telegraph-content-box").Each(func(i int, selection *goquery.Selection) {
		//logger.SugaredLogger.Info(selection.Text())
		telegraph = append(telegraph, selection.Text())
	})
	return &telegraph
}

// isTradingDay 判断是否是交易日
func isTradingDay(date time.Time) bool {
	weekday := date.Weekday()
	// 判断是否是周末
	if weekday == time.Saturday || weekday == time.Sunday {
		return false
	}
	// 这里可以添加具体的节假日判断逻辑
	// 例如：判断是否是春节、国庆节等
	return true
}

// isTradingTime 判断是否是交易时间
func isTradingTime(date time.Time) bool {
	if !isTradingDay(date) {
		return false
	}

	hour, minute, _ := date.Clock()

	// 判断是否在9:15到11:30之间
	if (hour == 9 && minute >= 15) || (hour == 10) || (hour == 11 && minute <= 30) {
		return true
	}

	// 判断是否在13:00到15:00之间
	if (hour == 13) || (hour == 14) || (hour == 15 && minute <= 0) {
		return true
	}

	return false
}

// IsHKTradingTime 判断当前时间是否在港股交易时间内
func IsHKTradingTime(date time.Time) bool {
	hour, minute, _ := date.Clock()

	// 开市前竞价时段：09:00 - 09:30
	if (hour == 9 && minute >= 0) || (hour == 9 && minute <= 30) {
		return true
	}

	// 上午持续交易时段：09:30 - 12:00
	if (hour == 9 && minute > 30) || (hour >= 10 && hour < 12) || (hour == 12 && minute == 0) {
		return true
	}

	// 下午持续交易时段：13:00 - 16:00
	if (hour == 13 && minute >= 0) || (hour >= 14 && hour < 16) || (hour == 16 && minute == 0) {
		return true
	}

	// 收市竞价交易时段：16:00 - 16:10
	if (hour == 16 && minute >= 0) || (hour == 16 && minute <= 10) {
		return true
	}
	return false
}

// IsUSTradingTime 判断当前时间是否在美股交易时间内
func IsUSTradingTime(date time.Time) bool {
	// 获取美国东部时区
	est, err := time.LoadLocation("America/New_York")
	var estTime time.Time
	if err != nil {
		estTime = date.Add(time.Hour * -12)
	} else {
		// 将当前时间转换为美国东部时间
		estTime = date.In(est)
	}

	// 判断是否是周末
	weekday := estTime.Weekday()
	if weekday == time.Saturday || weekday == time.Sunday {
		return false
	}

	// 获取小时和分钟
	hour, minute, _ := estTime.Clock()

	// 判断是否在4:00 AM到9:30 AM之间（盘前）
	if (hour == 4) || (hour == 5) || (hour == 6) || (hour == 7) || (hour == 8) || (hour == 9 && minute < 30) {
		return true
	}

	// 判断是否在9:30 AM到4:00 PM之间（盘中）
	if (hour == 9 && minute >= 30) || (hour >= 10 && hour < 16) || (hour == 16 && minute == 0) {
		return true
	}

	// 判断是否在4:00 PM到8:00 PM之间（盘后）
	if (hour == 16 && minute > 0) || (hour >= 17 && hour < 20) || (hour == 20 && minute == 0) {
		return true
	}

	return false
}
func MonitorFundPrices(a *App) {
	dest := &[]data.FollowedFund{}
	db.Dao.Model(&data.FollowedFund{}).Find(dest)
	for _, follow := range *dest {
		_, err := data.NewFundApi().CrawlFundBasic(follow.Code)
		if err != nil {
			logger.SugaredLogger.Errorf("获取基金基本信息失败，基金代码：%s，错误信息：%s", follow.Code, err.Error())
			continue
		}
		data.NewFundApi().CrawlFundNetEstimatedUnit(follow.Code)
		data.NewFundApi().CrawlFundNetUnitValue(follow.Code)
	}
}

func MonitorStockPrices(a *App) {
	dest := &[]data.FollowedStock{}
	db.Dao.Model(&data.FollowedStock{}).Find(dest)
	total := float64(0)
	//for _, follow := range *dest {
	//	stockData := getStockInfo(follow)
	//	total += stockData.ProfitAmountToday
	//	price, _ := convertor.ToFloat(stockData.Price)
	//	if stockData.PrePrice != price {
	//		go runtime.EventsEmit(a.ctx, "stock_price", stockData)
	//	}
	//}

	stockInfos := GetStockInfos(*dest...)
	for _, stockInfo := range *stockInfos {
		if strutil.HasPrefixAny(stockInfo.Code, []string{"SZ", "SH", "sh", "sz"}) && (!isTradingTime(time.Now())) {
			continue
		}
		if strutil.HasPrefixAny(stockInfo.Code, []string{"hk", "HK"}) && (!IsHKTradingTime(time.Now())) {
			continue
		}
		if strutil.HasPrefixAny(stockInfo.Code, []string{"us", "US", "gb_"}) && (!IsUSTradingTime(time.Now())) {
			continue
		}

		total += stockInfo.ProfitAmountToday
		price, _ := convertor.ToFloat(stockInfo.Price)
		if stockInfo.PrePrice != price {
			logger.SugaredLogger.Infof("-----------------------股票代码: %s, 股票名称: %s, 股票价格: %s,盘前盘后:%s", stockInfo.Code, stockInfo.Name, stockInfo.Price, stockInfo.BA)
			go runtime.EventsEmit(a.ctx, "stock_price", stockInfo)
		}
	}
	if total != 0 {
		title := "go-stock " + time.Now().Format(time.DateTime) + fmt.Sprintf("  %.2f¥", total)
		systray.SetTooltip(title)
	}

	go runtime.EventsEmit(a.ctx, "realtime_profit", fmt.Sprintf("  %.2f", total))
	//runtime.WindowSetTitle(a.ctx, title)

}
func GetStockInfos(follows ...data.FollowedStock) *[]data.StockInfo {
	stockInfos := make([]data.StockInfo, 0)
	stockCodes := make([]string, 0)
	for _, follow := range follows {
		if strutil.HasPrefixAny(follow.StockCode, []string{"SZ", "SH", "sh", "sz"}) && (!isTradingTime(time.Now())) {
			continue
		}
		if strutil.HasPrefixAny(follow.StockCode, []string{"hk", "HK"}) && (!IsHKTradingTime(time.Now())) {
			continue
		}
		if strutil.HasPrefixAny(follow.StockCode, []string{"us", "US", "gb_"}) && (!IsUSTradingTime(time.Now())) {
			continue
		}
		stockCodes = append(stockCodes, follow.StockCode)
	}
	stockData, _ := data.NewStockDataApi().GetStockCodeRealTimeData(stockCodes...)
	for _, info := range *stockData {
		v, ok := slice.FindBy(follows, func(idx int, follow data.FollowedStock) bool {
			if strutil.HasPrefixAny(follow.StockCode, []string{"US", "us"}) {
				return strings.ToLower(strings.Replace(follow.StockCode, "us", "gb_", 1)) == info.Code
			}

			return follow.StockCode == info.Code
		})
		if ok {
			addStockFollowData(v, &info)
			stockInfos = append(stockInfos, info)
		}
	}
	return &stockInfos
}
func getStockInfo(follow data.FollowedStock) *data.StockInfo {
	stockCode := follow.StockCode
	stockDatas, err := data.NewStockDataApi().GetStockCodeRealTimeData(stockCode)
	if err != nil || len(*stockDatas) == 0 {
		return &data.StockInfo{}
	}
	stockData := (*stockDatas)[0]
	addStockFollowData(follow, &stockData)
	return &stockData
}

func addStockFollowData(follow data.FollowedStock, stockData *data.StockInfo) {
	stockData.PrePrice = follow.Price //上次当前价格
	stockData.Sort = follow.Sort
	stockData.CostPrice = follow.CostPrice //成本价
	stockData.CostVolume = follow.Volume   //成本量
	stockData.AlarmChangePercent = follow.AlarmChangePercent
	stockData.AlarmPrice = follow.AlarmPrice

	//当前价格
	price, _ := convertor.ToFloat(stockData.Price)
	//当前价格为0 时 使用卖一价格作为当前价格
	if price == 0 {
		price, _ = convertor.ToFloat(stockData.A1P)
	}
	//当前价格依然为0 时 使用买一报价作为当前价格
	if price == 0 {
		price, _ = convertor.ToFloat(stockData.B1P)
	}

	//昨日收盘价
	preClosePrice, _ := convertor.ToFloat(stockData.PreClose)

	//当前价格依然为0 时 使用昨日收盘价为当前价格
	if price == 0 {
		price = preClosePrice
	}

	//今日最高价
	highPrice, _ := convertor.ToFloat(stockData.High)
	if highPrice == 0 {
		highPrice, _ = convertor.ToFloat(stockData.Open)
	}

	//今日最低价
	lowPrice, _ := convertor.ToFloat(stockData.Low)
	if lowPrice == 0 {
		lowPrice, _ = convertor.ToFloat(stockData.Open)
	}
	//开盘价
	//openPrice, _ := convertor.ToFloat(stockData.Open)

	if price > 0 && preClosePrice > 0 {
		stockData.ChangePrice = mathutil.RoundToFloat(price-preClosePrice, 2)
		stockData.ChangePercent = mathutil.RoundToFloat(mathutil.Div(price-preClosePrice, preClosePrice)*100, 3)
	}
	if highPrice > 0 && preClosePrice > 0 {
		stockData.HighRate = mathutil.RoundToFloat(mathutil.Div(highPrice-preClosePrice, preClosePrice)*100, 3)
	}
	if lowPrice > 0 && preClosePrice > 0 {
		stockData.LowRate = mathutil.RoundToFloat(mathutil.Div(lowPrice-preClosePrice, preClosePrice)*100, 3)
	}
	if follow.CostPrice > 0 && follow.Volume > 0 {
		if price > 0 {
			stockData.Profit = mathutil.RoundToFloat(mathutil.Div(price-follow.CostPrice, follow.CostPrice)*100, 3)
			stockData.ProfitAmount = mathutil.RoundToFloat((price-follow.CostPrice)*float64(follow.Volume), 2)
			stockData.ProfitAmountToday = mathutil.RoundToFloat((price-preClosePrice)*float64(follow.Volume), 2)
		} else {
			//未开盘时当前价格为昨日收盘价
			stockData.Profit = mathutil.RoundToFloat(mathutil.Div(preClosePrice-follow.CostPrice, follow.CostPrice)*100, 3)
			stockData.ProfitAmount = mathutil.RoundToFloat((preClosePrice-follow.CostPrice)*float64(follow.Volume), 2)
			stockData.ProfitAmountToday = mathutil.RoundToFloat((preClosePrice-preClosePrice)*float64(follow.Volume), 2)
		}

	}

	//logger.SugaredLogger.Debugf("stockData:%+v", stockData)
	if follow.Price != price && price > 0 {
		go db.Dao.Model(follow).Where("stock_code = ?", follow.StockCode).Updates(map[string]interface{}{
			"price": price,
		})
	}
}

// beforeClose is called when the application is about to quit,
// either by clicking the window close button or calling runtime.Quit.
// Returning true will cause the application to continue, false will continue shutdown as normal.
func (a *App) beforeClose(ctx context.Context) (prevent bool) {
	defer PanicHandler()

	dialog, err := runtime.MessageDialog(ctx, runtime.MessageDialogOptions{
		Type:         runtime.QuestionDialog,
		Title:        "go-stock",
		Message:      "确定关闭吗？",
		Buttons:      []string{"确定"},
		Icon:         icon,
		CancelButton: "取消",
	})

	if err != nil {
		logger.SugaredLogger.Errorf("dialog error:%s", err.Error())
		return false
	}
	logger.SugaredLogger.Debugf("dialog:%s", dialog)
	if dialog == "No" {
		return true
	}
	return false
}

// shutdown is called at application termination
func (a *App) shutdown(ctx context.Context) {
	defer PanicHandler()
	// Perform your teardown here
	systray.Quit()
	a.cron.Stop()
	os.Exit(0)
}

// Greet returns a greeting for the given name
func (a *App) Greet(stockCode string) *data.StockInfo {
	//stockInfo, _ := data.NewStockDataApi().GetStockCodeRealTimeData(stockCode)

	follow := &data.FollowedStock{
		StockCode: stockCode,
	}
	db.Dao.Model(follow).Where("stock_code = ?", stockCode).First(follow)
	stockInfo := getStockInfo(*follow)
	return stockInfo
}

func (a *App) Follow(stockCode string) string {
	return data.NewStockDataApi().Follow(stockCode)
}

func (a *App) UnFollow(stockCode string) string {
	return data.NewStockDataApi().UnFollow(stockCode)
}

func (a *App) GetFollowList() *[]data.FollowedStock {
	return data.NewStockDataApi().GetFollowList()
}

func (a *App) GetStockList(key string) []data.StockBasic {
	return data.NewStockDataApi().GetStockList(key)
}

func (a *App) SetCostPriceAndVolume(stockCode string, price float64, volume int64) string {
	return data.NewStockDataApi().SetCostPriceAndVolume(price, volume, stockCode)
}

func (a *App) SetAlarmChangePercent(val, alarmPrice float64, stockCode string) string {
	return data.NewStockDataApi().SetAlarmChangePercent(val, alarmPrice, stockCode)
}
func (a *App) SetStockSort(sort int64, stockCode string) {
	data.NewStockDataApi().SetStockSort(sort, stockCode)
}
func (a *App) SendDingDingMessage(message string, stockCode string) string {
	ttl, _ := a.cache.TTL([]byte(stockCode))
	logger.SugaredLogger.Infof("stockCode %s ttl:%d", stockCode, ttl)
	if ttl > 0 {
		return ""
	}
	err := a.cache.Set([]byte(stockCode), []byte("1"), 60*5)
	if err != nil {
		logger.SugaredLogger.Errorf("set cache error:%s", err.Error())
		return ""
	}
	return data.NewDingDingAPI().SendDingDingMessage(message)
}

// SendDingDingMessageByType msgType 报警类型: 1 涨跌报警;2 股价报警 3 成本价报警
func (a *App) SendDingDingMessageByType(message string, stockCode string, msgType int) string {

	if strutil.HasPrefixAny(stockCode, []string{"SZ", "SH", "sh", "sz"}) && (!isTradingTime(time.Now())) {
		return "非A股交易时间"
	}
	if strutil.HasPrefixAny(stockCode, []string{"hk", "HK"}) && (!IsHKTradingTime(time.Now())) {
		return "非港股交易时间"
	}
	if strutil.HasPrefixAny(stockCode, []string{"us", "US", "gb_"}) && (!IsUSTradingTime(time.Now())) {
		return "非美股交易时间"
	}

	ttl, _ := a.cache.TTL([]byte(stockCode))
	//logger.SugaredLogger.Infof("stockCode %s ttl:%d", stockCode, ttl)
	if ttl > 0 {
		return ""
	}
	err := a.cache.Set([]byte(stockCode), []byte("1"), getMsgTypeTTL(msgType))
	if err != nil {
		logger.SugaredLogger.Errorf("set cache error:%s", err.Error())
		return ""
	}
	stockInfo := &data.StockInfo{}
	db.Dao.Model(stockInfo).Where("code = ?", stockCode).First(stockInfo)
	go data.NewAlertWindowsApi("go-stock消息通知", getMsgTypeName(msgType), GenNotificationMsg(stockInfo), "").SendNotification()
	return data.NewDingDingAPI().SendDingDingMessage(message)
}

func (a *App) NewChatStream(stock, stockCode, question string, sysPromptId *int) {
	msgs := data.NewDeepSeekOpenAi(a.ctx).NewChatStream(stock, stockCode, question, sysPromptId)
	for msg := range msgs {
		runtime.EventsEmit(a.ctx, "newChatStream", msg)
	}
	runtime.EventsEmit(a.ctx, "newChatStream", "DONE")
}

func (a *App) SaveAIResponseResult(stockCode, stockName, result, chatId, question string) {
	data.NewDeepSeekOpenAi(a.ctx).SaveAIResponseResult(stockCode, stockName, result, chatId, question)
}
func (a *App) GetAIResponseResult(stock string) *models.AIResponseResult {
	return data.NewDeepSeekOpenAi(a.ctx).GetAIResponseResult(stock)
}

func (a *App) GetVersionInfo() *models.VersionInfo {
	return &models.VersionInfo{
		Version: Version,
		Icon:    GetImageBase(icon),
		Alipay:  GetImageBase(alipay),
		Wxpay:   GetImageBase(wxpay),
		Content: VersionCommit,
	}
}

// checkChromeOnWindows 在 Windows 系统上检查谷歌浏览器是否安装
func checkChromeOnWindows() bool {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe`, registry.QUERY_VALUE)
	if err != nil {
		// 尝试在 WOW6432Node 中查找（适用于 64 位系统上的 32 位程序）
		key, err = registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe`, registry.QUERY_VALUE)
		if err != nil {
			return false
		}
		defer key.Close()
	}
	defer key.Close()
	_, _, err = key.GetValue("Path", nil)
	return err == nil
}

// checkEdgeOnWindows 在 Windows 系统上检查Edge浏览器是否安装，并返回安装路径
func checkEdgeOnWindows() (string, bool) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe`, registry.QUERY_VALUE)
	if err != nil {
		// 尝试在 WOW6432Node 中查找（适用于 64 位系统上的 32 位程序）
		key, err = registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe`, registry.QUERY_VALUE)
		if err != nil {
			return "", false
		}
		defer key.Close()
	}
	defer key.Close()
	path, _, err := key.GetStringValue("Path")
	if err != nil {
		return "", false
	}
	return path, true
}

func GetImageBase(bytes []byte) string {
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(bytes)
}

func GenNotificationMsg(stockInfo *data.StockInfo) string {
	Price, err := convertor.ToFloat(stockInfo.Price)
	if err != nil {
		Price = 0
	}
	PreClose, err := convertor.ToFloat(stockInfo.PreClose)
	if err != nil {
		PreClose = 0
	}
	var RF float64
	if PreClose > 0 {
		RF = mathutil.RoundToFloat(((Price-PreClose)/PreClose)*100, 2)
	}

	return "[" + stockInfo.Name + "] " + stockInfo.Price + " " + convertor.ToString(RF) + "% " + stockInfo.Date + " " + stockInfo.Time
}

// msgType : 1 涨跌报警(5分钟);2 股价报警(30分钟) 3 成本价报警(30分钟)
func getMsgTypeTTL(msgType int) int {
	switch msgType {
	case 1:
		return 60 * 5
	case 2:
		return 60 * 30
	case 3:
		return 60 * 30
	default:
		return 60 * 5
	}
}

func getMsgTypeName(msgType int) string {
	switch msgType {
	case 1:
		return "涨跌报警"
	case 2:
		return "股价报警"
	case 3:
		return "成本价报警"
	default:
		return "未知类型"
	}
}

func onExit(a *App) {
	// 清理操作
	logger.SugaredLogger.Infof("onExit")
	systray.Quit()
	runtime.Quit(a.ctx)
}

func onReady(a *App) {

	// 初始化操作
	logger.SugaredLogger.Infof("onReady")
	systray.SetIcon(icon2)
	systray.SetTitle("go-stock")
	systray.SetTooltip("go-stock 股票行情实时获取")
	// 创建菜单项
	show := systray.AddMenuItem("显示", "显示应用程序")
	show.Click(func() {
		//logger.SugaredLogger.Infof("显示应用程序")
		runtime.WindowShow(a.ctx)
	})
	hide := systray.AddMenuItem("隐藏", "隐藏应用程序")
	hide.Click(func() {
		//logger.SugaredLogger.Infof("隐藏应用程序")
		runtime.WindowHide(a.ctx)
	})
	systray.AddSeparator()
	mQuitOrig := systray.AddMenuItem("退出", "退出应用程序")
	mQuitOrig.Click(func() {
		//logger.SugaredLogger.Infof("退出应用程序")
		runtime.Quit(a.ctx)
	})
	systray.SetOnRClick(func(menu systray.IMenu) {
		menu.ShowMenu()
		//logger.SugaredLogger.Infof("SetOnRClick")
	})
	systray.SetOnClick(func(menu systray.IMenu) {
		//logger.SugaredLogger.Infof("SetOnClick")
		menu.ShowMenu()
	})
	systray.SetOnDClick(func(menu systray.IMenu) {
		menu.ShowMenu()
		//logger.SugaredLogger.Infof("SetOnDClick")
	})
}

func (a *App) UpdateConfig(settings *data.Settings) string {
	//logger.SugaredLogger.Infof("UpdateConfig:%+v", settings)
	return data.NewSettingsApi(settings).UpdateConfig()
}

func (a *App) GetConfig() *data.Settings {
	return data.NewSettingsApi(&data.Settings{}).GetConfig()
}

func (a *App) ExportConfig() string {
	config := data.NewSettingsApi(&data.Settings{}).Export()
	file, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:                "导出配置文件",
		CanCreateDirectories: true,
		DefaultFilename:      "config.json",
	})
	if err != nil {
		logger.SugaredLogger.Errorf("导出配置文件失败:%s", err.Error())
		return err.Error()
	}
	err = os.WriteFile(file, []byte(config), 0644)
	if err != nil {
		logger.SugaredLogger.Errorf("导出配置文件失败:%s", err.Error())
		return err.Error()
	}
	return "导出成功:" + file
}
func getScreenResolution() (int, int, error) {
	//user32 := syscall.NewLazyDLL("user32.dll")
	//getSystemMetrics := user32.NewProc("GetSystemMetrics")
	//
	//width, _, _ := getSystemMetrics.Call(0)
	//height, _, _ := getSystemMetrics.Call(1)

	return int(1366), int(768), nil
}

func (a *App) ShareAnalysis(stockCode, stockName string) string {
	//http://go-stock.sparkmemory.top:16688/upload
	res := data.NewDeepSeekOpenAi(a.ctx).GetAIResponseResult(stockCode)
	if res != nil && len(res.Content) > 100 {
		analysisTime := res.CreatedAt.Format("2006/01/02")
		logger.SugaredLogger.Infof("%s analysisTime:%s", res.CreatedAt, analysisTime)
		response, err := resty.New().SetHeader("ua-x", "go-stock").R().SetFormData(map[string]string{
			"text":         res.Content,
			"stockCode":    stockCode,
			"stockName":    stockName,
			"analysisTime": analysisTime,
		}).Post("http://go-stock.sparkmemory.top:16688/upload")
		if err != nil {
			return err.Error()
		}
		return response.String()
	} else {
		return "分析结果异常"
	}
}

func (a *App) GetfundList(key string) []data.FundBasic {
	return data.NewFundApi().GetFundList(key)
}
func (a *App) GetFollowedFund() []data.FollowedFund {
	return data.NewFundApi().GetFollowedFund()
}
func (a *App) FollowFund(fundCode string) string {
	return data.NewFundApi().FollowFund(fundCode)
}
func (a *App) UnFollowFund(fundCode string) string {
	return data.NewFundApi().UnFollowFund(fundCode)
}
func (a *App) SaveAsMarkdown(stockCode, stockName string) string {
	res := data.NewDeepSeekOpenAi(a.ctx).GetAIResponseResult(stockCode)
	if res != nil && len(res.Content) > 100 {
		analysisTime := res.CreatedAt.Format("2006-01-02_15_04_05")
		file, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
			Title:           "保存为Markdown",
			DefaultFilename: fmt.Sprintf("%s[%s]AI分析结果_%s.md", stockName, stockCode, analysisTime),
			Filters: []runtime.FileFilter{
				{
					DisplayName: "Markdown",
					Pattern:     "*.md;*.markdown",
				},
			},
		})
		if err != nil {
			return err.Error()
		}
		err = os.WriteFile(file, []byte(res.Content), 0644)
		return "已保存至：" + file
	}
	return "分析结果异常,无法保存。"
}

func (a *App) GetPromptTemplates(name, promptType string) *[]models.PromptTemplate {
	return data.NewPromptTemplateApi().GetPromptTemplates(name, promptType)
}
func (a *App) AddPrompt(prompt models.Prompt) string {
	promptTemplate := models.PromptTemplate{
		ID:      prompt.ID,
		Content: prompt.Content,
		Name:    prompt.Name,
		Type:    prompt.Type,
	}
	return data.NewPromptTemplateApi().AddPrompt(promptTemplate)
}
func (a *App) DelPrompt(id uint) string {
	return data.NewPromptTemplateApi().DelPrompt(id)
}
func (a *App) SetStockAICron(cronText, stockCode string) {
	data.NewStockDataApi().SetStockAICron(cronText, stockCode)
	if strutil.HasPrefixAny(stockCode, []string{"gb_"}) {
		stockCode = strings.ToUpper(stockCode)
		stockCode = strings.Replace(stockCode, "gb_", "us", 1)
		stockCode = strings.Replace(stockCode, "GB_", "us", 1)
	}
	if entryID, exists := a.cronEntrys[stockCode]; exists {
		a.cron.Remove(entryID)
	}
	follow := data.NewStockDataApi().GetFollowedStockByStockCode(stockCode)
	a.cron.AddFunc(cronText, a.AddCronTask(follow))
}
func OnSecondInstanceLaunch(secondInstanceData options.SecondInstanceData) {
	notification := toast.Notification{
		AppID:    "go-stock",
		Title:    "go-stock",
		Message:  "程序已经在运行了",
		Icon:     "",
		Duration: "short",
		Audio:    toast.Default,
	}
	err := notification.Push()
	if err != nil {
		logger.SugaredLogger.Error(err)
	}
	time.Sleep(time.Second * 3)
}
