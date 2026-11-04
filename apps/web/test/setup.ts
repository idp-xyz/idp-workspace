import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';

// Vitest does not expose globals, so Testing Library cannot register its own cleanup.
afterEach(() => cleanup());
