# Flutter 示例前端

这是一个独立的 Flutter 客户端示例，只读取 Go 后端的最新 Feed，未修改 `frontend/`（Vue）或后端代码。

## 功能

- 调用 `POST /feed/listLatest` 显示最新视频
- 下拉刷新和顶部刷新按钮
- 网络错误与空列表状态
- 展示视频封面、标题、作者及点赞数

## 运行

先启动原项目的依赖和 Go 后端：

```powershell
docker compose up -d mysql redis rabbitmq
cd backend
$env:CONFIG_PATH = 'configs/config.compose-local.yaml'
go run ./cmd
```

首次使用 Flutter 时，若目录尚没有平台工程，在本目录执行一次：

```powershell
flutter create .
flutter pub get
```

再启动应用：

```powershell
# Android 模拟器：默认地址已是 http://10.0.2.2:8080
flutter run

# Windows 桌面端
flutter run -d windows --dart-define=API_BASE_URL=http://127.0.0.1:8080

# Android 真机：改成运行 Go 后端的电脑局域网 IP
flutter run --dart-define=API_BASE_URL=http://192.168.1.10:8080
```

真机和电脑必须处于同一局域网；不要在真机上使用 `localhost`。Android 若使用明文 HTTP，还需要在生成后的 `android/app/src/main/AndroidManifest.xml` 的 `<application>` 添加 `android:usesCleartextTraffic="true"`。

> Flutter Web 会受浏览器 CORS 限制。这个示例优先面向 Android 模拟器、真机和桌面端；这些客户端不需要给 Go 后端配置 CORS。
