import { reactive } from 'vue'
import { ApiError } from '../api/client'
import * as likeApi from '../api/like'
import type { FeedVideoItem } from '../api/types'
import { useAuthStore } from '../stores/auth'
import { useSocialStore } from '../stores/social'
import { useToastStore } from '../stores/toast'

export function useLikeFollow(needLogin: () => void) {
  const auth = useAuthStore()
  const social = useSocialStore()
  const toast = useToastStore()

  const likeBusy = reactive<Record<string, boolean>>({})
  const followBusy = reactive<Record<string, boolean>>({})

  async function toggleLike(item: FeedVideoItem) {
    if (!auth.isLoggedIn) return needLogin()
    const key = String(item.id)
    if (likeBusy[key]) return
    likeBusy[key] = true
    try {
      const wasLiked = item.is_liked
      const res = wasLiked ? await likeApi.unlike(item.id) : await likeApi.like(item.id)
      // 服务端幂等命中时（例如另一个标签页已经点过），状态要纠正，但计数不能再动一次。
      const already = wasLiked ? res?.already_unliked : res?.already_liked
      item.is_liked = !wasLiked
      if (!already) item.likes_count = Math.max(0, item.likes_count + (item.is_liked ? 1 : -1))
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : String(e)
      toast.error(msg)
    } finally {
      likeBusy[key] = false
    }
  }

  async function toggleFollow(authorId: number) {
    if (!auth.isLoggedIn) return needLogin()
    const key = String(authorId)
    if (followBusy[key]) return
    followBusy[key] = true
    try {
      if (social.isFollowing(authorId)) {
        await social.unfollow(authorId)
        toast.info('已取关')
      } else {
        await social.follow(authorId)
        toast.success('已关注')
      }
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : String(e)
      toast.error(msg)
    } finally {
      followBusy[key] = false
    }
  }

  async function share(item: FeedVideoItem) {
    const url = `${location.origin}/video/${item.id}`
    try {
      await navigator.clipboard.writeText(url)
      toast.success('链接已复制')
    } catch {
      window.prompt('复制链接', url)
    }
  }

  return { likeBusy, followBusy, toggleLike, toggleFollow, share }
}
