import { test, expect } from '@playwright/test';

async function createInstance(page: import('@playwright/test').Page, name: string) {
  await page.goto('/instances/new');
  await page.getByLabel('Instance Name *').fill(name);
  await page.getByRole('button', { name: /Create & Start Instance/i }).click();
  await expect(page).toHaveURL('/', { timeout: 30000 });
}

test.describe('Instance Lifecycle', () => {
  test.describe('Create Instance', () => {
    test('should show loading state on create button when submitting', async ({ page }) => {
      await page.goto('/instances/new');
      await expect(page.getByRole('heading', { name: 'Create New Instance' })).toBeVisible();

      const submitBtn = page.getByRole('button', { name: /Create & Start Instance/i });
      await expect(submitBtn).toBeVisible();
      await expect(submitBtn).toBeEnabled();

      const spinner = submitBtn.locator('.spinner');
      await expect(spinner).toBeHidden();

      await page.getByLabel('Instance Name *').fill('test-loading-' + Date.now());

      await page.route('**/instances', async route => {
        await new Promise(resolve => setTimeout(resolve, 1000));
        await route.continue();
      });

      await submitBtn.click();

      await expect(submitBtn).toBeDisabled({ timeout: 2000 });
      await expect(spinner).toBeVisible({ timeout: 2000 });
    });

    test('should create instance and redirect to dashboard', async ({ page }) => {
      const name = 'test-create-' + Date.now();
      await createInstance(page, name);

      const row = page.locator(`tr:has-text("${name}")`);
      await expect(row).toBeVisible({ timeout: 5000 });
    });

    test('should reject duplicate instance names', async ({ page }) => {
      const name = 'test-dup-' + Date.now();

      await createInstance(page, name);

      await page.goto('/instances/new');
      await page.getByLabel('Instance Name *').fill(name);
      await page.getByRole('button', { name: /Create & Start Instance/i }).click();

      await expect(page.locator('.toast')).toBeVisible({ timeout: 5000 });
    });
  });

  test.describe('Dashboard Button Loading', () => {
    test('action buttons should have hidden spinners by default', async ({ page }) => {
      const name = 'test-spinner-' + Date.now();
      await createInstance(page, name);

      const row = page.locator(`tr:has-text("${name}")`);
      await expect(row).toBeVisible({ timeout: 5000 });

      const spinners = row.locator('.spinner');
      const count = await spinners.count();
      expect(count).toBeGreaterThan(0);

      for (let i = 0; i < count; i++) {
        await expect(spinners.nth(i)).toBeHidden();
      }
    });
  });

  test.describe('Delete Instance', () => {
    test('should delete instance and remove row from dashboard', async ({ page }) => {
      const name = 'test-delete-' + Date.now();
      await createInstance(page, name);

      const row = page.locator(`tr:has-text("${name}")`);
      await expect(row).toBeVisible({ timeout: 5000 });

      page.on('dialog', dialog => dialog.accept());

      const deleteBtn = row.getByRole('button', { name: /Delete/i });
      await deleteBtn.click();

      await expect(row).toHaveCount(0, { timeout: 15000 });
    });

    test('should not show "instance not found" toast after deletion', async ({ page }) => {
      const name = 'test-no-toast-' + Date.now();
      await createInstance(page, name);

      const row = page.locator(`tr:has-text("${name}")`);
      await expect(row).toBeVisible({ timeout: 5000 });

      page.on('dialog', dialog => dialog.accept());

      const deleteBtn = row.getByRole('button', { name: /Delete/i });
      await deleteBtn.click();

      await page.waitForTimeout(12000);

      const toasts = page.locator('.toast');
      const toastCount = await toasts.count();
      for (let i = 0; i < toastCount; i++) {
        const text = await toasts.nth(i).textContent();
        expect(text?.toLowerCase()).not.toContain('not found');
      }
    });

    test('should not show error toast after deletion', async ({ page }) => {
      const name = 'test-no-err-' + Date.now();
      await createInstance(page, name);

      const row = page.locator(`tr:has-text("${name}")`);
      await expect(row).toBeVisible({ timeout: 5000 });

      page.on('dialog', dialog => dialog.accept());

      const deleteBtn = row.getByRole('button', { name: /Delete/i });
      await deleteBtn.click();

      await page.waitForTimeout(3000);

      const toastError = page.locator('.toast-error');
      await expect(toastError).toHaveCount(0);
    });
  });
});
