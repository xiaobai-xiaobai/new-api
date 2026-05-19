package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/Calcium-Ion/go-epay/epay"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func GetTopUpInfo(c *gin.Context) {
	// 获取支付方式
	payMethods := operation_setting.PayMethods

	// 如果启用了 Stripe 支付，添加到支付方法列表
	if setting.StripeApiSecret != "" && setting.StripeWebhookSecret != "" && setting.StripePriceId != "" {
		// 检查是否已经包含 Stripe
		hasStripe := false
		for _, method := range payMethods {
			if method["type"] == "stripe" {
				hasStripe = true
				break
			}
		}

		if !hasStripe {
			stripeMethod := map[string]string{
				"name":      "Stripe",
				"type":      "stripe",
				"color":     "rgba(var(--semi-purple-5), 1)",
				"min_topup": strconv.Itoa(setting.StripeMinTopUp),
			}
			payMethods = append(payMethods, stripeMethod)
		}
	}

	data := gin.H{
		"enable_online_topup": operation_setting.PayAddress != "" && operation_setting.EpayId != "" && operation_setting.EpayKey != "",
		"enable_stripe_topup": setting.StripeApiSecret != "" && setting.StripeWebhookSecret != "" && setting.StripePriceId != "",
		"enable_creem_topup":  setting.CreemApiKey != "" && setting.CreemProducts != "[]",
		"creem_products":      setting.CreemProducts,
		"pay_methods":         payMethods,
		"min_topup":           operation_setting.MinTopUp,
		"stripe_min_topup":    setting.StripeMinTopUp,
		"amount_options":      operation_setting.GetPaymentSetting().AmountOptions,
		"discount":            operation_setting.GetPaymentSetting().AmountDiscount,
	}
	common.ApiSuccess(c, data)
}

type EpayRequest struct {
	Amount        int64  `json:"amount"`
	PaymentMethod string `json:"payment_method"`
}

type AmountRequest struct {
	Amount int64 `json:"amount"`
}

func GetEpayClient() *epay.Client {
	if operation_setting.PayAddress == "" || operation_setting.EpayId == "" || operation_setting.EpayKey == "" {
		return nil
	}
	withUrl, err := epay.NewClient(&epay.Config{
		PartnerID: operation_setting.EpayId,
		Key:       operation_setting.EpayKey,
	}, operation_setting.PayAddress)
	if err != nil {
		return nil
	}
	return withUrl
}

func getPayMoney(amount int64, group string) float64 {
	dAmount := decimal.NewFromInt(amount)
	// 充值金额以“展示类型”为准：
	// - USD/CNY: 前端传 amount 为金额单位；TOKENS: 前端传 tokens，需要换成 USD 金额
	if operation_setting.GetQuotaDisplayType() == operation_setting.QuotaDisplayTypeTokens {
		dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
		dAmount = dAmount.Div(dQuotaPerUnit)
	}

	topupGroupRatio := common.GetTopupGroupRatio(group)
	if topupGroupRatio == 0 {
		topupGroupRatio = 1
	}

	dTopupGroupRatio := decimal.NewFromFloat(topupGroupRatio)
	dPrice := decimal.NewFromFloat(operation_setting.Price)
	// apply optional preset discount by the original request amount (if configured), default 1.0
	discount := 1.0
	if ds, ok := operation_setting.GetPaymentSetting().AmountDiscount[int(amount)]; ok {
		if ds > 0 {
			discount = ds
		}
	}
	dDiscount := decimal.NewFromFloat(discount)

	payMoney := dAmount.Mul(dPrice).Mul(dTopupGroupRatio).Mul(dDiscount)

	return payMoney.InexactFloat64()
}

func getMinTopup() int64 {
	minTopup := operation_setting.MinTopUp
	if operation_setting.GetQuotaDisplayType() == operation_setting.QuotaDisplayTypeTokens {
		dMinTopup := decimal.NewFromInt(int64(minTopup))
		dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
		minTopup = int(dMinTopup.Mul(dQuotaPerUnit).IntPart())
	}
	return int64(minTopup)
}

func RequestEpay(c *gin.Context) {
	var req EpayRequest
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.JSON(200, gin.H{"message": "error", "data": "参数错误"})
		return
	}
	if req.Amount < getMinTopup() {
		c.JSON(200, gin.H{"message": "error", "data": fmt.Sprintf("充值数量不能小于 %d", getMinTopup())})
		return
	}
	// ✅ 添加最大充值金额限制(防止恶意大额充值)
	maxTopup := int64(1000000000) // 默认最大10亿额度
	if operation_setting.GetQuotaDisplayType() == operation_setting.QuotaDisplayTypeTokens {
		maxTopup = maxTopup * int64(common.QuotaPerUnit)
	}
	if req.Amount > maxTopup {
		c.JSON(200, gin.H{"message": "error", "data": fmt.Sprintf("充值数量不能超过 %d", maxTopup)})
		return
	}

	id := c.GetInt("id")
	group, err := model.GetUserGroup(id, true)
	if err != nil {
		c.JSON(200, gin.H{"message": "error", "data": "获取用户分组失败"})
		return
	}
	payMoney := getPayMoney(req.Amount, group)
	if payMoney < 0.01 {
		c.JSON(200, gin.H{"message": "error", "data": "充值金额过低"})
		return
	}
	// ✅ 添加支付金额合理性检查(防止异常金额)
	if payMoney > 100000 { // 单笔支付不超过10万元
		log.Printf("⚠️ 异常充值请求: user_id=%d, amount=%d, payMoney=%.2f", id, req.Amount, payMoney)
		c.JSON(200, gin.H{"message": "error", "data": "充值金额超出限制"})
		return
	}

	if !operation_setting.ContainsPayMethod(req.PaymentMethod) {
		c.JSON(200, gin.H{"message": "error", "data": "支付方式不存在"})
		return
	}

	callBackAddress := service.GetCallbackAddress()
	returnUrl, _ := url.Parse(system_setting.ServerAddress + "/console/log")
	notifyUrl, _ := url.Parse(callBackAddress + "/api/user/epay/notify")
	tradeNo := fmt.Sprintf("%s%d", common.GetRandomString(6), time.Now().Unix())
	tradeNo = fmt.Sprintf("USR%dNO%s", id, tradeNo)
	client := GetEpayClient()
	if client == nil {
		c.JSON(200, gin.H{"message": "error", "data": "当前管理员未配置支付信息"})
		return
	}
	uri, params, err := client.Purchase(&epay.PurchaseArgs{
		Type:           req.PaymentMethod,
		ServiceTradeNo: tradeNo,
		Name:           fmt.Sprintf("TUC%d", req.Amount),
		Money:          strconv.FormatFloat(payMoney, 'f', 2, 64),
		Device:         epay.PC,
		NotifyUrl:      notifyUrl,
		ReturnUrl:      returnUrl,
	})
	if err != nil {
		c.JSON(200, gin.H{"message": "error", "data": "拉起支付失败"})
		return
	}
	amount := req.Amount
	if operation_setting.GetQuotaDisplayType() == operation_setting.QuotaDisplayTypeTokens {
		dAmount := decimal.NewFromInt(int64(amount))
		dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
		amount = dAmount.Div(dQuotaPerUnit).IntPart()
	}
	topUp := &model.TopUp{
		UserId:        id,
		Amount:        amount,
		Money:         payMoney,
		TradeNo:       tradeNo,
		PaymentMethod: req.PaymentMethod,
		CreateTime:    time.Now().Unix(),
		Status:        "pending",
	}
	err = topUp.Insert()
	if err != nil {
		c.JSON(200, gin.H{"message": "error", "data": "创建订单失败"})
		return
	}
	c.JSON(200, gin.H{"message": "success", "data": params, "url": uri})
}

// tradeNo lock
var orderLocks sync.Map
var createLock sync.Mutex

// LockOrder 尝试对给定订单号加锁
func LockOrder(tradeNo string) {
	lock, ok := orderLocks.Load(tradeNo)
	if !ok {
		createLock.Lock()
		defer createLock.Unlock()
		lock, ok = orderLocks.Load(tradeNo)
		if !ok {
			lock = new(sync.Mutex)
			orderLocks.Store(tradeNo, lock)
		}
	}
	lock.(*sync.Mutex).Lock()
}

// UnlockOrder 释放给定订单号的锁
func UnlockOrder(tradeNo string) {
	lock, ok := orderLocks.Load(tradeNo)
	if ok {
		lock.(*sync.Mutex).Unlock()
	}
}

func EpayNotify(c *gin.Context) {
	var params map[string]string

	if c.Request.Method == "POST" {
		// POST 请求：从 POST body 解析参数
		if err := c.Request.ParseForm(); err != nil {
			log.Println("易支付回调POST解析失败:", err)
			_, _ = c.Writer.Write([]byte("fail"))
			return
		}
		params = lo.Reduce(lo.Keys(c.Request.PostForm), func(r map[string]string, t string, i int) map[string]string {
			r[t] = c.Request.PostForm.Get(t)
			return r
		}, map[string]string{})
	} else {
		// GET 请求：从 URL Query 解析参数
		params = lo.Reduce(lo.Keys(c.Request.URL.Query()), func(r map[string]string, t string, i int) map[string]string {
			r[t] = c.Request.URL.Query().Get(t)
			return r
		}, map[string]string{})
	}

	if len(params) == 0 {
		log.Println("易支付回调参数为空")
		_, _ = c.Writer.Write([]byte("fail"))
		return
	}
	client := GetEpayClient()
	if client == nil {
		log.Println("易支付回调失败 未找到配置信息")
		_, err := c.Writer.Write([]byte("fail"))
		if err != nil {
			log.Println("易支付回调写入失败")
		}
		return
	}
	verifyInfo, err := client.Verify(params)
	if err == nil && verifyInfo.VerifyStatus {
		_, err := c.Writer.Write([]byte("success"))
		if err != nil {
			log.Println("易支付回调写入失败")
		}
	} else {
		_, err := c.Writer.Write([]byte("fail"))
		if err != nil {
			log.Println("易支付回调写入失败")
		}
		log.Println("易支付回调签名验证失败")
		return
	}

	if verifyInfo.TradeStatus == epay.StatusTradeSuccess {
		log.Printf("📥 易支付回调: %+v", verifyInfo)
		LockOrder(verifyInfo.ServiceTradeNo)
		defer UnlockOrder(verifyInfo.ServiceTradeNo)
		topUp := model.GetTopUpByTradeNo(verifyInfo.ServiceTradeNo)
		if topUp == nil {
			log.Printf("❌ 易支付回调未找到订单: %v", verifyInfo)
			return
		}
		if topUp.Status == "pending" {
			// ✅ 二次验证:主动查询易支付服务器确认订单状态
			log.Printf("🔍 开始二次验证订单: %s", verifyInfo.ServiceTradeNo)
			queryResult, queryErr := queryEpayOrder(client, verifyInfo.ServiceTradeNo)
			if queryErr != nil {
				log.Printf("⚠️ 查询易支付订单失败: %v, 订单号:%s", queryErr, verifyInfo.ServiceTradeNo)
				// 查询失败时标记为可疑,需要人工审核
				topUp.Status = "suspicious"
				topUp.Update()
				model.RecordLog(topUp.UserId, model.LogTypeError,
					fmt.Sprintf("易支付二次验证失败! 订单号:%s, 错误:%v",
						verifyInfo.ServiceTradeNo, queryErr))
				return
			}

			// 验证查询结果中的订单状态
			queryTradeStatus := queryResult["trade_status"]
			if queryTradeStatus != epay.StatusTradeSuccess {
				log.Printf("⚠️ 易支付服务器返回订单状态异常! 订单号:%s, 状态:%s",
					verifyInfo.ServiceTradeNo, queryTradeStatus)
				topUp.Status = "suspicious"
				topUp.Update()
				model.RecordLog(topUp.UserId, model.LogTypeError,
					fmt.Sprintf("易支付二次验证状态异常! 订单号:%s, 回调状态:%s, 查询状态:%s",
						verifyInfo.ServiceTradeNo, verifyInfo.TradeStatus, queryTradeStatus))
				return
			}

			// 验证查询结果中的金额
			queryMoneyStr := queryResult["money"]
			queryMoney, err := strconv.ParseFloat(queryMoneyStr, 64)
			if err != nil {
				log.Printf("⚠️ 查询结果金额格式错误! 订单号:%s, 金额:%s", verifyInfo.ServiceTradeNo, queryMoneyStr)
				topUp.Status = "suspicious"
				topUp.Update()
				model.RecordLog(topUp.UserId, model.LogTypeError,
					fmt.Sprintf("易支付二次验证金额格式错误! 订单号:%s, 金额:%s",
						verifyInfo.ServiceTradeNo, queryMoneyStr))
				return
			}

			// 对比回调金额和查询金额是否一致
			callbackMoneyStr := verifyInfo.Money
			callbackMoney, err := strconv.ParseFloat(callbackMoneyStr, 64)
			if err != nil {
				log.Printf("⚠️ 回调金额格式错误! 订单号:%s, 回调金额:%s", verifyInfo.ServiceTradeNo, callbackMoneyStr)
				return
			}

			moneyDiff := queryMoney - callbackMoney
			if moneyDiff < -0.01 || moneyDiff > 0.01 {
				log.Printf("⚠️ 回调金额与查询金额不一致! 订单号:%s, 回调金额:%.2f, 查询金额:%.2f, 差额:%.2f",
					verifyInfo.ServiceTradeNo, callbackMoney, queryMoney, moneyDiff)
				topUp.Status = "suspicious"
				topUp.Update()
				model.RecordLog(topUp.UserId, model.LogTypeError,
					fmt.Sprintf("易支付二次验证金额不一致! 订单号:%s, 回调:%.2f, 查询:%.2f",
						verifyInfo.ServiceTradeNo, callbackMoney, queryMoney))
				return
			}

			log.Printf("✅ 二次验证通过: 订单号=%s, 状态=%s, 金额=%.2f",
				verifyInfo.ServiceTradeNo, queryTradeStatus, queryMoney)

			// ✅ 安全校验:验证回调金额与订单金额是否一致
			expectedMoney := topUp.Money
			// 允许0.01元的误差(浮点数精度问题)
			moneyDiff = callbackMoney - expectedMoney
			if moneyDiff < -0.01 || moneyDiff > 0.01 {
				log.Printf("⚠️ 金额不匹配! 订单号:%s, 预期金额:%.2f, 回调金额:%.2f, 差额:%.2f",
					verifyInfo.ServiceTradeNo, expectedMoney, callbackMoney, moneyDiff)
				// ⚠️ 标记为可疑订单,不自动完成
				topUp.Status = "suspicious"
				topUp.Update()
				model.RecordLog(topUp.UserId, model.LogTypeError,
					fmt.Sprintf("易支付回调金额异常! 订单号:%s, 预期:%.2f, 实际:%.2f",
						verifyInfo.ServiceTradeNo, expectedMoney, callbackMoney))
				return
			}

			// ✅ 二次校验:验证充值额度是否在合理范围内
			dAmount := decimal.NewFromInt(int64(topUp.Amount))
			dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
			quotaToAdd := int(dAmount.Mul(dQuotaPerUnit).IntPart())

			// 单次充值不超过10亿额度
			if quotaToAdd > 1000000000 {
				log.Printf("⚠️ 充值额度过大! 订单号:%s, 额度:%d", verifyInfo.ServiceTradeNo, quotaToAdd)
				topUp.Status = "suspicious"
				topUp.Update()
				model.RecordLog(topUp.UserId, model.LogTypeError,
					fmt.Sprintf("易支付回调额度过大! 订单号:%s, 额度:%d",
						verifyInfo.ServiceTradeNo, quotaToAdd))
				return
			}

			// ✅ 使用事务保证原子性
			if err = model.DB.Transaction(func(tx *gorm.DB) error {
				// 更新订单状态
				topUp.Status = "success"
				topUp.CompleteTime = common.GetTimestamp()
				if err := tx.Save(topUp).Error; err != nil {
					return err
				}

				// 增加用户额度
				if err := tx.Model(&model.User{}).Where("id = ?", topUp.UserId).
					Update("quota", gorm.Expr("quota + ?", quotaToAdd)).Error; err != nil {
					return err
				}

				return nil
			}); err != nil {
				log.Printf("❌ 易支付回调处理失败: %v, 订单: %v", err, topUp)
				return
			}
			log.Printf("✅ 易支付充值成功: 订单号=%s, 用户ID=%d, 充值额度=%d, 支付金额=%.2f",
				verifyInfo.ServiceTradeNo, topUp.UserId, quotaToAdd, topUp.Money)
			model.RecordLog(topUp.UserId, model.LogTypeTopup,
				fmt.Sprintf("使用在线充值成功，充值金额: %v，支付金额：%.2f",
					logger.LogQuota(quotaToAdd), topUp.Money))

			// ✅ 安全检查:新用户或未消费用户充值,发送告警邮件给管理员
			checkAndNotifySuspiciousTopup(topUp.UserId, quotaToAdd, topUp.Money, verifyInfo.ServiceTradeNo, topUp.PaymentMethod)
		}
	} else {
		log.Printf("⚠️ 易支付异常回调: %v", verifyInfo)
	}
}

func RequestAmount(c *gin.Context) {
	var req AmountRequest
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.JSON(200, gin.H{"message": "error", "data": "参数错误"})
		return
	}

	if req.Amount < getMinTopup() {
		c.JSON(200, gin.H{"message": "error", "data": fmt.Sprintf("充值数量不能小于 %d", getMinTopup())})
		return
	}
	id := c.GetInt("id")
	group, err := model.GetUserGroup(id, true)
	if err != nil {
		c.JSON(200, gin.H{"message": "error", "data": "获取用户分组失败"})
		return
	}
	payMoney := getPayMoney(req.Amount, group)
	if payMoney <= 0.01 {
		c.JSON(200, gin.H{"message": "error", "data": "充值金额过低"})
		return
	}
	c.JSON(200, gin.H{"message": "success", "data": strconv.FormatFloat(payMoney, 'f', 2, 64)})
}

func GetUserTopUps(c *gin.Context) {
	userId := c.GetInt("id")
	pageInfo := common.GetPageQuery(c)
	keyword := c.Query("keyword")

	var (
		topups []*model.TopUp
		total  int64
		err    error
	)
	if keyword != "" {
		topups, total, err = model.SearchUserTopUps(userId, keyword, pageInfo)
	} else {
		topups, total, err = model.GetUserTopUps(userId, pageInfo)
	}
	if err != nil {
		common.ApiError(c, err)
		return
	}

	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(topups)
	common.ApiSuccess(c, pageInfo)
}

// GetAllTopUps 管理员获取全平台充值记录
func GetAllTopUps(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	keyword := c.Query("keyword")

	var (
		topups []*model.TopUp
		total  int64
		err    error
	)
	if keyword != "" {
		topups, total, err = model.SearchAllTopUps(keyword, pageInfo)
	} else {
		topups, total, err = model.GetAllTopUps(pageInfo)
	}
	if err != nil {
		common.ApiError(c, err)
		return
	}

	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(topups)
	common.ApiSuccess(c, pageInfo)
}

type AdminCompleteTopupRequest struct {
	TradeNo string `json:"trade_no"`
}

// AdminCompleteTopUp 管理员补单接口
func AdminCompleteTopUp(c *gin.Context) {
	var req AdminCompleteTopupRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.TradeNo == "" {
		common.ApiErrorMsg(c, "参数错误")
		return
	}

	// 订单级互斥，防止并发补单
	LockOrder(req.TradeNo)
	defer UnlockOrder(req.TradeNo)

	if err := model.ManualCompleteTopUp(req.TradeNo); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

// queryEpayOrder 主动查询易支付订单状态（二次验证）
func queryEpayOrder(client *epay.Client, tradeNo string) (map[string]string, error) {
	// 构造查询参数
	params := map[string]string{
		"out_trade_no": tradeNo,
		"pid":          operation_setting.EpayId,
	}

	// 生成签名
	keys := make([]string, 0, len(params))
	for k := range params {
		if params[k] != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	signStr := ""
	for _, k := range keys {
		signStr += k + "=" + params[k] + "&"
	}
	signStr += operation_setting.EpayKey

	// 使用 MD5 签名
	params["sign"] = common.MD5(signStr)
	params["sign_type"] = "MD5"

	// 构造查询 URL
	queryURL := operation_setting.PayAddress + "/api.php?act=order"

	// 发送 HTTP POST 请求
	formData := url.Values{}
	for k, v := range params {
		formData.Set(k, v)
	}
	resp, err := http.PostForm(queryURL, formData)
	if err != nil {
		return nil, fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	// 解析响应为 map[string]string
	var result map[string]string
	if err := json.Unmarshal(body, &result); err != nil {
		// 如果不是 JSON，尝试解析为 key=value 格式
		result = make(map[string]string)
		pairs := string(body)
		for _, pair := range splitQuery(pairs) {
			kv := splitKeyValue(pair)
			if len(kv) == 2 {
				result[kv[0]] = kv[1]
			}
		}
	}

	return result, nil
}

// splitQuery 将 key1=value1&key2=value2 格式的字符串分割成数组
func splitQuery(query string) []string {
	result := []string{}
	start := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '&' {
			if i > start {
				result = append(result, query[start:i])
			}
			start = i + 1
		}
	}
	if start < len(query) {
		result = append(result, query[start:])
	}
	return result
}

// splitKeyValue 将 key=value 格式的字符串分割成 [key, value]
func splitKeyValue(pair string) []string {
	for i := 0; i < len(pair); i++ {
		if pair[i] == '=' {
			return []string{pair[:i], pair[i+1:]}
		}
	}
	return []string{pair}
}

// checkAndNotifySuspiciousTopup 检查是否为可疑充值并发送告警邮件
func checkAndNotifySuspiciousTopup(userId int, quota int, money float64, tradeNo string, paymentMethod string) {
	// 获取用户信息
	user, err := model.GetUserById(userId, false)
	if err != nil || user == nil {
		log.Printf("⚠️ 无法获取用户信息: userId=%d, err=%v", userId, err)
		return
	}

	// 检查1: 用户注册时间是否在24小时内(从数据库查询创建时间)
	now := time.Now().Unix()
	var registrationTime int64
	model.DB.Model(&model.User{}).Where("id = ?", userId).Select("created_at").Scan(&registrationTime)
	if registrationTime == 0 {
		// 如果 created_at 不存在,尝试使用 id 估算(假设 id 越小注册越早)
		var maxId int
		model.DB.Model(&model.User{}).Select("COALESCE(MAX(id), 1)").Scan(&maxId)
		if maxId > 0 && userId <= maxId/10 {
			// 如果是前10%的用户,认为是老用户
			registrationTime = now - 86400*30 // 假设30天前
		} else {
			registrationTime = now // 新用户
		}
	}
	hoursSinceRegistration := float64(now-registrationTime) / 3600.0

	isNewUser := hoursSinceRegistration < 24

	// 检查2: 用户是否有消费记录(查询 LogTypeConsume 类型的日志)
	var consumeCount int64
	model.DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", userId, model.LogTypeConsume).Count(&consumeCount)
	hasConsumed := consumeCount > 0

	// 如果新用户或未消费就充值,发送告警邮件
	if isNewUser || !hasConsumed {
		reason := ""
		if isNewUser && !hasConsumed {
			reason = fmt.Sprintf("新用户(注册%.1f小时前)且无消费记录", hoursSinceRegistration)
		} else if isNewUser {
			reason = fmt.Sprintf("新用户(注册%.1f小时前)", hoursSinceRegistration)
		} else {
			reason = "无消费记录"
		}

		// 构建告警邮件内容
		subject := fmt.Sprintf("【充值告警】%s - %s", reason, user.Username)
		content := fmt.Sprintf(`
			<h2>🚨 可疑充值告警</h2>
			<hr/>
			<h3>用户信息</h3>
			<ul>
				<li><strong>用户名:</strong> %s</li>
				<li><strong>用户ID:</strong> %d</li>
				<li><strong>邮箱:</strong> %s</li>
				<li><strong>注册时间:</strong> %s (%.1f小时前)</li>
				<li><strong>当前角色:</strong> %d</li>
			</ul>
			
			<h3>充值信息</h3>
			<ul>
				<li><strong>订单号:</strong> %s</li>
				<li><strong>支付方式:</strong> %s</li>
				<li><strong>充值额度:</strong> %s</li>
				<li><strong>支付金额:</strong> %.2f 元</li>
				<li><strong>充值时间:</strong> %s</li>
			</ul>
			
			<h3>消费情况</h3>
			<ul>
				<li><strong>历史消费次数:</strong> %d 次</li>
				<li><strong>是否新用户:</strong> %v (注册%.1f小时)</li>
				<li><strong>是否有消费:</strong> %v</li>
			</ul>
			
			<h3>告警原因</h3>
			<p style="color: red; font-weight: bold;">%s</p>
			
			<hr/>
			<p style="color: gray; font-size: 12px;">此邮件由系统自动发送，请及时核实该充值的合法性。</p>
		`,
			user.Username,
			userId,
			user.Email,
			time.Unix(registrationTime, 0).Format("2006-01-02 15:04:05"),
			hoursSinceRegistration,
			user.Role,
			tradeNo,
			paymentMethod,
			logger.LogQuota(quota),
			money,
			time.Unix(now, 0).Format("2006-01-02 15:04:05"),
			consumeCount,
			isNewUser,
			hoursSinceRegistration,
			hasConsumed,
			reason,
		)

		// 发送给管理员(root用户)
		rootUser := model.GetRootUser()
		if rootUser != nil && rootUser.Email != "" {
			err := common.SendEmail(subject, rootUser.Email, content)
			if err != nil {
				log.Printf("❌ 发送告警邮件失败: %v", err)
			} else {
				log.Printf("✅ 已发送告警邮件给管理员: %s, 原因: %s", rootUser.Email, reason)
			}
		}

		// 同时记录到数据库日志
		model.RecordLog(userId, model.LogTypeError,
			fmt.Sprintf("可疑充值告警: %s, 订单:%s, 额度:%s, 金额:%.2f",
				reason, tradeNo, logger.LogQuota(quota), money))
	}
}
