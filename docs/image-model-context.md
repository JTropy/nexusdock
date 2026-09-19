# 在 ChatGPT 中查看节点图片

部分 ChatGPT 网页会话收到标准 MCP `image` 块后，只让模型访问元数据，不能据此判断模型已看到像素。NexusDock 为 `view_image` 提供一个可选 MCP App 图片组件。

## 使用

1. 开启 NexusDock 的 MCP Apps，在 ChatGPT 插件设置刷新工具，并新建会话。
2. 调用目标节点的 `view_image`。组件显示这一次工具结果中的真实图片。
3. 点击“让 ChatGPT 查看图片”。组件调用宿主 `uploadFile`，将真实文件 ID 写入 `widgetState.imageIds`，然后请求后续识图回复。

这是**用户点击后、后续轮次**的视觉输入，不是无人值守的同轮 Computer Use。没有对应宿主接口时，只提供图片预览；不能声称模型看到图片。宿主没有 `sendFollowUpMessage` 时，上传完成后由用户继续提问。

## 数据与兼容边界

- 保留节点 `outputSchema`、`structuredContent` 和标准图片 `content`，其他 MCP 客户端仍可直接消费图片。
- 组件只处理本次结果的首个 PNG、JPEG、WebP 或 GIF 图片，不读取剪贴板、其他文件，也不从外部 URL 获取图片。
- 上传由用户点击触发，目标为当前 ChatGPT 宿主；不默认保存到文件库，不引入视觉 API 或 OCR。
- 图片预览使用内嵌数据，不新增 CSP 网络域名、不关闭认证。MCP Apps 关闭后不注入本组件的展示绑定。
- 重复点击复用已取得的文件 ID；上传失败可重试，上传途中换图不会把旧图附到新结果。
- 上传后的文件由 ChatGPT 管理，节点原 Artifact 的过期时间不等同于 ChatGPT 文件保留期限。

## 验证

`make ci` 包含 Go 的协议兼容/开关/资源测试，以及 Node 内置测试运行器执行的图片组件数据流测试（`make image-app-test`）。没有新增 npm 依赖。

人工端到端验收须使用 Chrome 与真实 ChatGPT 宿主：提示词只描述要观察的位置，不给出答案；禁止 OCR 或其他文件读取；在点击组件后核对至少一个无法靠常识或提示词猜出的视觉细节。单元测试和组件预览均不能替代此验收。

参考：[OpenAI 图像模型上下文说明](https://developers.openai.com/plugins/build/chatgpt-ui#make-images-visible-to-the-model)、[文件接口](https://developers.openai.com/plugins/reference#file-apis)。
