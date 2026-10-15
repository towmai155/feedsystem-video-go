import 'package:flutter/material.dart';

import 'config/api_config.dart';
import 'models/feed_video.dart';
import 'services/feed_api.dart';

void main() => runApp(const FeedSystemApp());

class FeedSystemApp extends StatelessWidget {
  const FeedSystemApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Video Feed',
      debugShowCheckedModeBanner: false,
      theme: ThemeData(
        useMaterial3: true,
        colorScheme: ColorScheme.fromSeed(
          seedColor: const Color(0xFFDE5B31),
          brightness: Brightness.dark,
        ),
        scaffoldBackgroundColor: const Color(0xFF151412),
      ),
      home: const FeedPage(),
    );
  }
}

class FeedPage extends StatefulWidget {
  const FeedPage({super.key});

  @override
  State<FeedPage> createState() => _FeedPageState();
}

class _FeedPageState extends State<FeedPage> {
  final _api = const FeedApi();
  late Future<List<FeedVideo>> _feed;

  @override
  void initState() {
    super.initState();
    _feed = _api.listLatest();
  }

  Future<void> _reload() async {
    setState(() => _feed = _api.listLatest());
    await _feed;
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: const Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text('VIDEO FEED', style: TextStyle(fontWeight: FontWeight.w800)),
            Text('来自 Go 服务的最新视频', style: TextStyle(fontSize: 12)),
          ],
        ),
        actions: [
          IconButton(
            tooltip: '刷新视频流',
            onPressed: _reload,
            icon: const Icon(Icons.refresh),
          ),
        ],
      ),
      body: FutureBuilder<List<FeedVideo>>(
        future: _feed,
        builder: (context, snapshot) {
          if (snapshot.connectionState != ConnectionState.done) {
            return const Center(child: CircularProgressIndicator());
          }
          if (snapshot.hasError) {
            return _ErrorState(error: snapshot.error.toString(), onRetry: _reload);
          }
          final videos = snapshot.data ?? const [];
          if (videos.isEmpty) {
            return const Center(child: Text('还没有可展示的视频。'));
          }
          return RefreshIndicator(
            onRefresh: _reload,
            child: ListView.separated(
              padding: const EdgeInsets.fromLTRB(16, 10, 16, 28),
              itemCount: videos.length,
              separatorBuilder: (_, __) => const SizedBox(height: 14),
              itemBuilder: (_, index) => _VideoCard(video: videos[index]),
            ),
          );
        },
      ),
    );
  }
}

class _VideoCard extends StatelessWidget {
  const _VideoCard({required this.video});

  final FeedVideo video;

  @override
  Widget build(BuildContext context) {
    final hasCover = video.coverUrl.startsWith('http');
    return Card(
      clipBehavior: Clip.antiAlias,
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          AspectRatio(
            aspectRatio: 16 / 9,
            child: hasCover
                ? Image.network(video.coverUrl, fit: BoxFit.cover, errorBuilder: (_, __, ___) => const _CoverPlaceholder())
                : const _CoverPlaceholder(),
          ),
          Padding(
            padding: const EdgeInsets.all(16),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(video.title, style: Theme.of(context).textTheme.titleLarge?.copyWith(fontWeight: FontWeight.w700)),
                if (video.description.isNotEmpty) ...[
                  const SizedBox(height: 6),
                  Text(video.description, maxLines: 2, overflow: TextOverflow.ellipsis),
                ],
                const SizedBox(height: 14),
                Row(
                  children: [
                    CircleAvatar(
                      radius: 14,
                      child: Text(
                        video.authorName.isEmpty ? '?' : video.authorName.substring(0, 1).toUpperCase(),
                      ),
                    ),
                    const SizedBox(width: 8),
                    Expanded(child: Text(video.authorName, overflow: TextOverflow.ellipsis)),
                    Icon(video.isLiked ? Icons.favorite : Icons.favorite_border, color: const Color(0xFFDE5B31), size: 18),
                    const SizedBox(width: 4),
                    Text('${video.likesCount}'),
                  ],
                ),
              ],
            ),
          ),
        ],
      ),
    );
  }
}

class _CoverPlaceholder extends StatelessWidget {
  const _CoverPlaceholder();

  @override
  Widget build(BuildContext context) => const ColoredBox(
        color: Color(0xFF2A2925),
        child: Center(child: Icon(Icons.play_circle_outline, size: 52, color: Color(0xFFDE5B31))),
      );
}

class _ErrorState extends StatelessWidget {
  const _ErrorState({required this.error, required this.onRetry});

  final String error;
  final Future<void> Function() onRetry;

  @override
  Widget build(BuildContext context) {
    return Center(
      child: Padding(
        padding: const EdgeInsets.all(24),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            const Icon(Icons.wifi_off_outlined, size: 44),
            const SizedBox(height: 12),
            const Text('无法加载视频流', style: TextStyle(fontSize: 20, fontWeight: FontWeight.bold)),
            const SizedBox(height: 8),
            Text(error, textAlign: TextAlign.center),
            const SizedBox(height: 18),
            FilledButton.icon(onPressed: onRetry, icon: const Icon(Icons.refresh), label: const Text('重新连接')),
            const SizedBox(height: 10),
            Text('当前 API：${ApiConfig.baseUrl}', textAlign: TextAlign.center, style: Theme.of(context).textTheme.bodySmall),
          ],
        ),
      ),
    );
  }
}
