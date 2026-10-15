import 'dart:convert';

import 'package:http/http.dart' as http;

import '../config/api_config.dart';
import '../models/feed_video.dart';

class FeedApi {
  const FeedApi();

  Future<List<FeedVideo>> listLatest() async {
    final response = await http.post(
      Uri.parse('${ApiConfig.baseUrl}/feed/listLatest'),
      headers: const {'Content-Type': 'application/json'},
      body: jsonEncode({'limit': 20, 'latest_time': 0}),
    );

    if (response.statusCode != 200) {
      throw FeedApiException('服务器返回 ${response.statusCode}：${response.body}');
    }

    final body = jsonDecode(response.body) as Map<String, dynamic>;
    final videos = body['video_list'] as List<dynamic>? ?? const [];
    return videos
        .whereType<Map<String, dynamic>>()
        .map(FeedVideo.fromJson)
        .toList();
  }
}

class FeedApiException implements Exception {
  const FeedApiException(this.message);

  final String message;

  @override
  String toString() => message;
}
