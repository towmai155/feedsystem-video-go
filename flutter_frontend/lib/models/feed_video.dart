class FeedVideo {
  const FeedVideo({
    required this.id,
    required this.authorName,
    required this.title,
    required this.description,
    required this.playUrl,
    required this.coverUrl,
    required this.likesCount,
    required this.isLiked,
  });

  final int id;
  final String authorName;
  final String title;
  final String description;
  final String playUrl;
  final String coverUrl;
  final int likesCount;
  final bool isLiked;

  factory FeedVideo.fromJson(Map<String, dynamic> json) {
    final author = json['author'] as Map<String, dynamic>? ?? const <String, dynamic>{};
    return FeedVideo(
      id: (json['id'] as num?)?.toInt() ?? 0,
      authorName: author['username'] as String? ?? '匿名用户',
      title: json['title'] as String? ?? '未命名视频',
      description: json['description'] as String? ?? '',
      playUrl: json['play_url'] as String? ?? '',
      coverUrl: json['cover_url'] as String? ?? '',
      likesCount: (json['likes_count'] as num?)?.toInt() ?? 0,
      isLiked: json['is_liked'] as bool? ?? false,
    );
  }
}
