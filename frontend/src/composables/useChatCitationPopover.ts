import type { Ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { getChunkByIdOnly } from '@/api/knowledge-base'
import type { CitationKnowledgeRef } from '@/utils/citationMarkdown'
import { useCitationPopover } from './useCitationPopover'

export { clearCitationChunkCache } from '@/utils/citationChunkCache'
export type { CitationFloatState } from './useCitationPopover'

export type ChatCitationPopoverOptions = {
  getKnowledgeReferences?: () => CitationKnowledgeRef[] | null | undefined
  sessionId?: () => string | undefined
}

export function useChatCitationPopover(rootRef: Ref<HTMLElement | null>, options?: ChatCitationPopoverOptions) {
  const { t } = useI18n()
  return useCitationPopover(rootRef, {
    mode: 'chat',
    getKnowledgeReferences: options?.getKnowledgeReferences,
    getCacheScope: () => options?.sessionId?.() || 'default',
    fetchChunk: (chunkId) => getChunkByIdOnly(chunkId),
    notFoundError: () => t('agentStream.citation.notFound'),
    loadError: () => t('agentStream.citation.loadFailed'),
  })
}
