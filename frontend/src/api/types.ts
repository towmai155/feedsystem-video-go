export type MessageResponse = {
  message: string
  /** 点赞幂等：服务端认为「已点过」，本次没有新增点赞记录，计数不应再 +1 */
  already_liked?: boolean
  /** 取消点赞幂等：服务端认为「本来就没点过」，计数不应再 -1 */
  already_unliked?: boolean
}

export type DirectMessage = {
  id: number
  from_id: number
  to_id: number
  content: string
  is_read: boolean
  created_at: string
}

export type ListMessagesResponse = {
  messages: DirectMessage[]
}

export type TokenResponse = { token: string; refresh_token?: string; account_id?: number; username?: string }

export type Account = {
  id: number
  username: string
  avatar_url?: string
  bio?: string
}

export type Video = {
  id: number
  author_id: number
  username: string
  title: string
  description?: string
  play_url: string
  cover_url: string
  create_time: string
  likes_count: number
}

export type Comment = {
  id: number
  username: string
  video_id: number
  author_id: number
  content: string
  created_at: string
}

export type FeedAuthor = {
  id: number
  username: string
}

export type FeedVideoItem = {
  id: number
  author: FeedAuthor
  title: string
  description?: string
  play_url: string
  cover_url: string
  create_time: number
  likes_count: number
  is_liked: boolean
}

export type ListLatestResponse = {
  video_list: FeedVideoItem[]
  next_time: number
  has_more: boolean
}

export type ListLikesCountResponse = {
  video_list: FeedVideoItem[]
  next_likes_count_before?: number
  next_id_before?: number
  has_more: boolean
}

export type ListByPopularityResponse = {
  video_list: FeedVideoItem[]
  as_of: number
  next_offset: number
  has_more: boolean
  next_latest_popularity?: number
  next_latest_before?: string
  next_latest_id_before?: number
}

export type ListByFollowingResponse = {
  video_list: FeedVideoItem[]
  next_time: number
  has_more: boolean
}

export type IsLikedResponse = {
  is_liked: boolean
}

export type GetAllFollowersResponse = {
  followers: Account[]
}

export type GetAllVloggersResponse = {
  vloggers: Account[]
}
