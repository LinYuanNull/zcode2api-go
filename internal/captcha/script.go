package captcha

import (
	"encoding/json"
	"strconv"
)

// 注入页面的两段脚本。
//
// 都是**本实现自己写的表达式**，只调用阿里 SDK 的公开接口
// （`window.initAliyunCaptcha` 的选项名、`#cap-holder` / `#cap-btn` 两个 id、
// `AliyunCaptchaConfig` 的 `{region, prefix}`）—— 这些名字由上游 SDK 定义，
// 不是谁家的实现细节。求解流程与参数语义见 PROVENANCE.md「A6 实现依据」。

// antiDetectJS 在**每个新文档的最早时机**抹掉最常见的自动化特征。
//
// 为什么需要它：无痕验证的判定目标就是「这是不是真浏览器」。puppeteer 时代的
// 三个显性特征（`navigator.webdriver`、语言列表只有一种、平台串与 UA 不一致）
// 会让风控直接把这次会话判成自动化 —— 判成自动化之后不是报错，而是**永远
// 不出码**，表现为「求解超时」，很难从现象反推原因。
//
// 只改这三项是刻意的：改得越多越容易与真实浏览器自相矛盾（例如伪造
// `navigator.plugins` 却忘了 `mimeTypes`），反而成为更强的特征。
const antiDetectJS = `Object.defineProperty(navigator, "webdriver", { get: () => undefined });
Object.defineProperty(navigator, "languages", { get: () => ["zh-CN", "zh", "en"] });
Object.defineProperty(navigator, "platform", { get: () => "Win32" });`

// 就绪判据（都是字符串表达式，避免为布尔结果再造一条 CDP 取数路径）。
const (
	// exprReadyState 取 document.readyState（loading / interactive / complete）。
	exprReadyState = `document.readyState`
	// exprSDKType 取 SDK 全局函数的类型；等于 "function" 才算加载完成。
	exprSDKType = `typeof window.initAliyunCaptcha`
)

// solveExpr 组装求解表达式：调 `initAliyunCaptcha` 走无痕验证，
// 成功回调里取 `captchaVerifyParam`，Promise 落定即返回该字符串。
//
// 三个参数都用 `json.Marshal` 引用，防止 scene/region/prefix 里的字符
// 破坏表达式（它们是配置值，可能被改）。
//
// `gateMS` 是**页面内**的兜底超时：即便 SDK 既不回调 success 也不回调 fail，
// 表达式也会在到期后落定成 null，从而让 CDP 调用正常返回而不是挂到协议超时。
// 取值必须**小于**外层 CDP 调用的超时。
func solveExpr(cfg Config, gateMS int) string {
	scene := jsQuote(cfg.SceneID)
	region := jsQuote(cfg.Region)
	prefix := jsQuote(cfg.Prefix)
	return `(function () {
  var state = { settled: false, timer: null };
  window.AliyunCaptchaConfig = { region: ` + region + `, prefix: ` + prefix + ` };
  return new Promise(function (resolve) {
    function done(value) {
      if (state.settled) { return; }
      state.settled = true;
      if (state.timer !== null) { clearTimeout(state.timer); }
      if (typeof value === "string") { resolve(value); return; }
      resolve((value && value.captchaVerifyParam) || null);
    }
    state.timer = setTimeout(function () { done(null); }, ` + strconv.Itoa(gateMS) + `);
    try {
      window.initAliyunCaptcha({
        SceneId: ` + scene + `,
        mode: "popup",
        language: "zh-CN",
        showErrorTip: false,
        element: "#cap-holder",
        button: "#cap-btn",
        getInstance: function (instance) {
          try { instance.startTracelessVerification(); }
          catch (err) { done(null); }
        },
        success: function (result) { done(result); },
        fail: function () { done(null); },
        onError: function () { done(null); }
      });
    } catch (err) { done(null); }
  });
})()`
}

// jsQuote 把字符串引用成 JS 字面量。
func jsQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal 对 string 不会失败；留一条兜底避免返回半截表达式。
		return `""`
	}
	return string(b)
}
