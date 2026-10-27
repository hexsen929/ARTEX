type MissingSkillNotice = {
  calls: number;
  library_present?: boolean;
};

export function missingSkillNoticeSummary(notices: readonly MissingSkillNotice[]) {
  return {
    skills: notices.length,
    calls: notices.reduce((sum, notice) => sum + Math.max(0, notice.calls), 0),
  };
}

export function missingSkillLibraryLabel(present: boolean | undefined): string {
  if (present === true) return "文件存在";
  if (present === false) return "文件缺失";
  return "文件状态未知";
}

// Never optimistically discard notices after acknowledging them: a concurrent
// invocation may have created a new notice after the server's acknowledgement.
export async function acknowledgeMissingSkillNotices<T>(
  acknowledge: () => Promise<unknown>,
  reload: () => Promise<T[]>,
): Promise<T[]> {
  await acknowledge();
  return reload();
}
