# Importing from zotero.org

gotero can copy your zotero.org library into your gotero account: collections, searches, items (including the trash), tags, settings, full-text content and, if you want, the attached files.

## 1. Create a zotero.org API key

1. Open <https://www.zotero.org/settings/keys> and choose "Create new private key".
2. Give it a name such as "gotero import".
3. Under "Personal library" tick **Allow library access** and **Allow notes access**. Tick **Allow file access** if you want the attachments. Write access is not needed.
4. Under "Default group permissions" choose **Read only** (or "Read/Write") so groups can be imported.
5. Save the key and copy it. You can delete the key on zotero.org after the import.

## 2. Start the import

**Web UI:** sign in to gotero, open the import page, paste the key, choose whether to include groups and files, and start. The page shows progress and you can cancel at any time.

**Command line**, on the machine that runs gotero:

```sh
echo "YOUR_ZOTERO_KEY" | gotero import --user alice --api-key-stdin
```

Add `--no-files` to skip attachments or `--no-groups` to skip group libraries. The command prints a progress line as it goes and exits with an error if the import fails. Press Ctrl-C to stop; the import resumes where it left off the next time it runs.

The key is stored only while the import runs and is deleted when it ends.

## What is imported

- Your personal library: collections, saved searches, items with their tags, relations and child notes, the trash, settings (such as tag colours) and full-text content.
- Into an empty library, object keys and versions are kept exactly as they are on zotero.org. See "Importing into a library that already has content" below for what happens otherwise.
- Files: attachments that zotero.org stores (stored files and snapshots). Linked files and links stay as links. Zipped snapshots are first saved to a temporary file on the server while they download, so the server's temp directory needs room for the largest snapshot.
- Groups, if you chose to include them: for every group your key can read, gotero creates a group with the **same group ID** and copies the name, description and settings. **You become the owner.** Other zotero.org members are not created or invited; add them again in gotero.

## What is not imported

- Other members of your groups, and group membership roles.
- Your zotero.org profile, API keys, account settings and storage subscription.
- Things you have deleted on zotero.org (the delete log is not copied).
- Files of linked attachments, which never lived on zotero.org.

## Running it again

An import can be repeated safely. Objects are matched by key: those that are new or changed on zotero.org are copied, those that are identical are left alone, and files already stored are not downloaded again. Objects that exist in gotero but not on zotero.org are **never deleted**, so work you did in gotero is kept.

### Importing into a library that already has content

The first import into an empty library keeps zotero.org's object versions, so a desktop that synced with zotero.org carries on without a full resync. If the import is interrupted and resumed, it still counts as that first import.

Every other import, including each rerun, works differently:

- Copied objects get new versions from gotero's own counter, like any edit made through the API. Versions are never lowered or reused, so a desktop that already synced with gotero picks the imported objects up on its next sync, even if your gotero library is further along than your zotero.org library.
- gotero remembers what it wrote for each object. A later import replaces an object only while it is still exactly as the import left it. If you edited or deleted it in gotero since, it is **kept as it is**, even when it changed on zotero.org too. The import counts these as conflicts, shows the number next to the library and ends with a warning. Nothing is lost, but those objects stay out of sync with zotero.org.
- An object with the same key that gotero already had before the import, and that differs from zotero.org's version, is treated the same way and kept.
- Settings and full-text content that gotero already holds are not replaced.
- A file is only attached when the item still describes it. If you replaced a file in gotero while the import was running, the old file is skipped and counted as a conflict.

If a group ID is already used by a group you own, it is imported into that group. If it belongs to someone else, that group is skipped (the import shows an error for it) and the rest continues.

If your storage quota runs out, the import stops downloading files, finishes the metadata and ends with a warning. Free some space or ask an admin for a larger quota, then run the import again.

## Keeping your zotero.org user ID

Zotero remembers your user ID. If your gotero account has the **same user ID as your zotero.org account**, desktop treats gotero as the same account and keeps working. Create the account with your zotero.org user ID (shown on <https://www.zotero.org/settings/keys>, "Your userID for use in API calls is ..."):

```sh
gotero user add alice --password-stdin --id 123456
```

Do this before you import. If the account already exists with another ID, create a new one rather than importing into it.

## After the import

Point Zotero desktop at gotero as described in [clients.md](clients.md), section "Moving from zotero.org". If you imported into an empty library, object versions were kept, so a desktop that last synced with zotero.org sees a consistent library and syncs only what changed.
